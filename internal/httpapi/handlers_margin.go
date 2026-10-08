package httpapi

import (
	"context"
	"net/http"

	"github.com/google/uuid"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/gen/api"
	"github.com/nodal/controlplane/internal/margin"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/observability"
	"github.com/nodal/controlplane/internal/security"
)

// The margin routes (internal/margin). Handlers translate shapes only: the
// margin service decides every figure, every status and every refusal, and
// its error codes reach the wire unchanged. Customer commands are the
// account owner's (accountScopeWrite); operator commands act on any account
// under sandbox:operate, and every one that moves value requires a step-up
// (authz.go).

const marginFeature = "margin"

// --- customer reads -----------------------------------------------------------

// GetAdvancedMarginAccount returns an account's margin account, evaluated by
// the margin engine at the instant of the read.
func (s *Server) GetAdvancedMarginAccount(ctx context.Context, request api.GetAdvancedMarginAccountRequestObject) (api.GetAdvancedMarginAccountResponseObject, error) {
	if s.opts.Ports.Margin == nil {
		return nil, errNotWired(marginFeature)
	}
	account, err := accountScope(ctx, request.Params.AccountId)
	if err != nil {
		return nil, err
	}
	v, err := s.opts.Ports.Margin.Account(ctx, account)
	if err != nil {
		return nil, err
	}
	out, err := toAPIMarginAccountDetail(v)
	if err != nil {
		return nil, err
	}
	return api.GetAdvancedMarginAccount200JSONResponse(out), nil
}

// GetAdvancedMarginCalls pages an account's margin calls.
func (s *Server) GetAdvancedMarginCalls(ctx context.Context, request api.GetAdvancedMarginCallsRequestObject) (api.GetAdvancedMarginCallsResponseObject, error) {
	if s.opts.Ports.Margin == nil {
		return nil, errNotWired(marginFeature)
	}
	account, err := accountScope(ctx, request.Params.AccountId)
	if err != nil {
		return nil, err
	}
	page, err := s.opts.Ports.Margin.Calls(ctx, account, marginCursorParam(request.Params.Cursor),
		pageLimit(request.Params.Limit, defaultPageLimit, maxPageLimit))
	if err != nil {
		return nil, err
	}
	return api.GetAdvancedMarginCalls200JSONResponse(toAPIMarginCallPage(page)), nil
}

// GetAdvancedMarginCases pages an account's liquidation cases.
func (s *Server) GetAdvancedMarginCases(ctx context.Context, request api.GetAdvancedMarginCasesRequestObject) (api.GetAdvancedMarginCasesResponseObject, error) {
	if s.opts.Ports.Margin == nil {
		return nil, errNotWired(marginFeature)
	}
	account, err := accountScope(ctx, request.Params.AccountId)
	if err != nil {
		return nil, err
	}
	page, err := s.opts.Ports.Margin.Cases(ctx, account, marginCursorParam(request.Params.Cursor),
		pageLimit(request.Params.Limit, defaultPageLimit, maxPageLimit))
	if err != nil {
		return nil, err
	}
	out := api.MarginCasePage{ValueDomain: api.SIMULATED, Items: make([]api.MarginCase, 0, len(page.Items)), NextCursor: nextCursor(page.NextCursor)}
	for _, c := range page.Items {
		out.Items = append(out.Items, toAPIMarginCase(c))
	}
	return api.GetAdvancedMarginCases200JSONResponse(out), nil
}

// GetAdvancedMarginCasesMarginCaseId returns one liquidation case with its
// steps to its account's owner (or an operator who may read any account). A
// case of another account answers exactly as one that does not exist.
func (s *Server) GetAdvancedMarginCasesMarginCaseId(ctx context.Context, request api.GetAdvancedMarginCasesMarginCaseIdRequestObject) (api.GetAdvancedMarginCasesMarginCaseIdResponseObject, error) {
	if s.opts.Ports.Margin == nil {
		return nil, errNotWired(marginFeature)
	}
	cid, err := margin.ParseCaseID(request.MarginCaseId.String())
	if err != nil || cid.IsZero() {
		return nil, validationError("marginCaseId", "marginCaseId must be a canonical UUID")
	}
	// One answer for "absent" and "not yours", built here, so the refusal
	// cannot tell a stranger the id exists (F-41).
	notFound := errs.New(errs.CodeNotFound, "no such liquidation case").WithField("case_id", cid.String())
	v, err := s.opts.Ports.Margin.Case(ctx, cid)
	if errs.HasCode(err, errs.CodeNotFound) {
		return nil, notFound
	}
	if err != nil {
		return nil, err
	}
	if err := security.RequireAccount(ctx, v.Facts.AccountID.String()); err != nil {
		return nil, notFound
	}
	return api.GetAdvancedMarginCasesMarginCaseId200JSONResponse(toAPIMarginCaseDetail(v)), nil
}

// GetAdvancedMarginStressRuns pages an account's stored stress runs.
func (s *Server) GetAdvancedMarginStressRuns(ctx context.Context, request api.GetAdvancedMarginStressRunsRequestObject) (api.GetAdvancedMarginStressRunsResponseObject, error) {
	if s.opts.Ports.Margin == nil {
		return nil, errNotWired(marginFeature)
	}
	account, err := accountScope(ctx, request.Params.AccountId)
	if err != nil {
		return nil, err
	}
	page, err := s.opts.Ports.Margin.StressRuns(ctx, account, marginCursorParam(request.Params.Cursor),
		pageLimit(request.Params.Limit, defaultPageLimit, maxPageLimit))
	if err != nil {
		return nil, err
	}
	out := api.MarginStressRunPage{ValueDomain: api.SIMULATED, Items: make([]api.MarginStressRunSummary, 0, len(page.Items)),
		NextCursor: nextCursor(page.NextCursor)}
	for _, r := range page.Items {
		out.Items = append(out.Items, toAPIMarginStressRunSummary(r))
	}
	return api.GetAdvancedMarginStressRuns200JSONResponse(out), nil
}

// GetAdvancedMarginStressRunsMarginStressRunId returns one stored stress run
// to its account's owner (or an operator who may read any account).
func (s *Server) GetAdvancedMarginStressRunsMarginStressRunId(ctx context.Context, request api.GetAdvancedMarginStressRunsMarginStressRunIdRequestObject) (api.GetAdvancedMarginStressRunsMarginStressRunIdResponseObject, error) {
	if s.opts.Ports.Margin == nil {
		return nil, errNotWired(marginFeature)
	}
	var rid margin.StressRunID
	if err := rid.UnmarshalText([]byte(request.MarginStressRunId.String())); err != nil || rid.IsZero() {
		return nil, validationError("marginStressRunId", "marginStressRunId must be a canonical UUID")
	}
	notFound := errs.New(errs.CodeNotFound, "no such stress run").WithField("stress_run_id", rid.String())
	v, err := s.opts.Ports.Margin.StressRun(ctx, rid)
	if errs.HasCode(err, errs.CodeNotFound) {
		return nil, notFound
	}
	if err != nil {
		return nil, err
	}
	if err := security.RequireAccount(ctx, v.Facts.AccountID.String()); err != nil {
		return nil, notFound
	}
	return api.GetAdvancedMarginStressRunsMarginStressRunId200JSONResponse(toAPIMarginStressRun(v)), nil
}

// --- customer commands ----------------------------------------------------------

// PostAdvancedMarginAccount opens the caller's margin account. The numeraire
// is the sandbox dollar; the service refuses any mode but PAPER and SANDBOX.
func (s *Server) PostAdvancedMarginAccount(ctx context.Context, request api.PostAdvancedMarginAccountRequestObject) (api.PostAdvancedMarginAccountResponseObject, error) {
	if s.opts.Ports.Margin == nil {
		return nil, errNotWired(marginFeature)
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	account, err := accountScopeWrite(ctx, request.Body.AccountId)
	if err != nil {
		return nil, err
	}
	mm := margin.MarginMode(request.Body.MarginMode)
	if !mm.Valid() {
		return nil, validationError("margin_mode", "margin_mode must be CASH, ISOLATED or CROSS")
	}
	mode := gates.Mode(request.Body.Mode)
	if !mode.Valid() {
		return nil, validationError("mode", "mode must be one of PAPER, SANDBOX, DEVNET, LIVE")
	}
	var isolated *assets.AssetID
	if request.Body.IsolatedAssetId != nil {
		aid, perr := assets.ParseAssetID(request.Body.IsolatedAssetId.String())
		if perr != nil || aid.IsZero() {
			return nil, validationError("isolated_asset_id", "isolated_asset_id must be a canonical UUID")
		}
		isolated = &aid
	}
	actor, err := marginActor(ctx)
	if err != nil {
		return nil, err
	}
	req := MarginOpen{AccountID: account, MarginMode: mm, Mode: mode, IsolatedAssetID: isolated, Actor: actor}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.MarginAccount, commandMeta, error) {
			a, oerr := s.opts.Ports.Margin.Open(ctx, req)
			if oerr != nil {
				return api.MarginAccount{}, commandMeta{}, oerr
			}
			return toAPIMarginAccount(a), commandMeta{Status: http.StatusOK, ResourceType: "margin_account", ResourceID: a.ID.String()}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedMarginAccount200JSONResponse(res.Value), nil
}

// PostAdvancedMarginAccountClose closes the caller's margin account.
func (s *Server) PostAdvancedMarginAccountClose(ctx context.Context, request api.PostAdvancedMarginAccountCloseRequestObject) (api.PostAdvancedMarginAccountCloseResponseObject, error) {
	if s.opts.Ports.Margin == nil {
		return nil, errNotWired(marginFeature)
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	account, err := accountScopeWrite(ctx, request.Body.AccountId)
	if err != nil {
		return nil, err
	}
	actor, err := marginActor(ctx)
	if err != nil {
		return nil, err
	}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.MarginAccount, commandMeta, error) {
			a, cerr := s.opts.Ports.Margin.Close(ctx, account, actor)
			if cerr != nil {
				return api.MarginAccount{}, commandMeta{}, cerr
			}
			return toAPIMarginAccount(a), commandMeta{Status: http.StatusOK, ResourceType: "margin_account", ResourceID: a.ID.String()}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedMarginAccountClose200JSONResponse(res.Value), nil
}

// PostAdvancedMarginCollateralPost moves collateral from the caller's wallet
// into the margin account.
func (s *Server) PostAdvancedMarginCollateralPost(ctx context.Context, request api.PostAdvancedMarginCollateralPostRequestObject) (api.PostAdvancedMarginCollateralPostResponseObject, error) {
	if s.opts.Ports.Margin == nil {
		return nil, errNotWired(marginFeature)
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	account, err := accountScopeWrite(ctx, request.Body.AccountId)
	if err != nil {
		return nil, err
	}
	req, err := marginCollateralRequest(ctx, account, *request.Body, request.Params.IdempotencyKey)
	if err != nil {
		return nil, err
	}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.MarginCollateralResult, commandMeta, error) {
			r, perr := s.opts.Ports.Margin.PostCollateral(ctx, req)
			if perr != nil {
				return api.MarginCollateralResult{}, commandMeta{}, perr
			}
			out, cerr := toAPIMarginCollateralResult(r)
			if cerr != nil {
				return api.MarginCollateralResult{}, commandMeta{}, cerr
			}
			return out, commandMeta{Status: http.StatusOK, ResourceType: "margin_collateral_move", ResourceID: r.MoveID.String()}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedMarginCollateralPost200JSONResponse(res.Value), nil
}

// PostAdvancedMarginCollateralRelease moves collateral out of the margin
// account to the caller's wallet; Risk Kernel V2 decides.
func (s *Server) PostAdvancedMarginCollateralRelease(ctx context.Context, request api.PostAdvancedMarginCollateralReleaseRequestObject) (api.PostAdvancedMarginCollateralReleaseResponseObject, error) {
	if s.opts.Ports.Margin == nil {
		return nil, errNotWired(marginFeature)
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	account, err := accountScopeWrite(ctx, request.Body.AccountId)
	if err != nil {
		return nil, err
	}
	req, err := marginCollateralRequest(ctx, account, *request.Body, request.Params.IdempotencyKey)
	if err != nil {
		return nil, err
	}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.MarginCollateralResult, commandMeta, error) {
			r, rerr := s.opts.Ports.Margin.ReleaseCollateral(ctx, req)
			if rerr != nil {
				return api.MarginCollateralResult{}, commandMeta{}, rerr
			}
			out, cerr := toAPIMarginCollateralResult(r)
			if cerr != nil {
				return api.MarginCollateralResult{}, commandMeta{}, cerr
			}
			return out, commandMeta{Status: http.StatusOK, ResourceType: "margin_collateral_move", ResourceID: r.MoveID.String()}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedMarginCollateralRelease200JSONResponse(res.Value), nil
}

// PostAdvancedMarginBorrow borrows the numeraire from the lending pool into
// the caller's wallet; Risk Kernel V2 decides.
func (s *Server) PostAdvancedMarginBorrow(ctx context.Context, request api.PostAdvancedMarginBorrowRequestObject) (api.PostAdvancedMarginBorrowResponseObject, error) {
	if s.opts.Ports.Margin == nil {
		return nil, errNotWired(marginFeature)
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	account, err := accountScopeWrite(ctx, request.Body.AccountId)
	if err != nil {
		return nil, err
	}
	req, err := marginLoanRequest(ctx, account, *request.Body, request.Params.IdempotencyKey)
	if err != nil {
		return nil, err
	}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.MarginLoanResult, commandMeta, error) {
			r, berr := s.opts.Ports.Margin.Borrow(ctx, req)
			if berr != nil {
				return api.MarginLoanResult{}, commandMeta{}, berr
			}
			out, cerr := toAPIMarginLoanResult(r)
			if cerr != nil {
				return api.MarginLoanResult{}, commandMeta{}, cerr
			}
			return out, commandMeta{Status: http.StatusOK, ResourceType: "economic_intent", ResourceID: r.IntentID.String()}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedMarginBorrow200JSONResponse(res.Value), nil
}

// PostAdvancedMarginRepay repays loans from the caller's wallet, interest
// first and oldest lot first, as the service allocates it.
func (s *Server) PostAdvancedMarginRepay(ctx context.Context, request api.PostAdvancedMarginRepayRequestObject) (api.PostAdvancedMarginRepayResponseObject, error) {
	if s.opts.Ports.Margin == nil {
		return nil, errNotWired(marginFeature)
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	account, err := accountScopeWrite(ctx, request.Body.AccountId)
	if err != nil {
		return nil, err
	}
	req, err := marginLoanRequest(ctx, account, *request.Body, request.Params.IdempotencyKey)
	if err != nil {
		return nil, err
	}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.MarginLoanResult, commandMeta, error) {
			r, rerr := s.opts.Ports.Margin.Repay(ctx, req)
			if rerr != nil {
				return api.MarginLoanResult{}, commandMeta{}, rerr
			}
			out, cerr := toAPIMarginLoanResult(r)
			if cerr != nil {
				return api.MarginLoanResult{}, commandMeta{}, cerr
			}
			return out, commandMeta{Status: http.StatusOK, ResourceType: "economic_intent", ResourceID: r.IntentID.String()}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedMarginRepay200JSONResponse(res.Value), nil
}

// PostAdvancedMarginLeveragedBuys buys an asset entirely on margin; Risk
// Kernel V2 decides.
func (s *Server) PostAdvancedMarginLeveragedBuys(ctx context.Context, request api.PostAdvancedMarginLeveragedBuysRequestObject) (api.PostAdvancedMarginLeveragedBuysResponseObject, error) {
	if s.opts.Ports.Margin == nil {
		return nil, errNotWired(marginFeature)
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	account, err := accountScopeWrite(ctx, request.Body.AccountId)
	if err != nil {
		return nil, err
	}
	asset, err := assets.ParseAssetID(request.Body.AssetId.String())
	if err != nil || asset.IsZero() {
		return nil, validationError("asset_id", "asset_id must be a canonical UUID")
	}
	qty, err := money.ParseQuantity(request.Body.Quantity)
	if err != nil {
		return nil, validationError("quantity", "quantity must be an integer string of the asset's base units")
	}
	requester, err := marginRequester(ctx)
	if err != nil {
		return nil, err
	}
	req := margin.SpotRequest{AccountID: account, AssetID: asset, Quantity: qty, IdempotencyKey: request.Params.IdempotencyKey,
		Requester: requester, CorrelationID: observability.CorrelationID(ctx)}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.MarginLeveragedBuyResult, commandMeta, error) {
			r, berr := s.opts.Ports.Margin.LeveragedBuy(ctx, req)
			if berr != nil {
				return api.MarginLeveragedBuyResult{}, commandMeta{}, berr
			}
			out, cerr := toAPIMarginLeveragedBuyResult(r)
			if cerr != nil {
				return api.MarginLeveragedBuyResult{}, commandMeta{}, cerr
			}
			return out, commandMeta{Status: http.StatusOK, ResourceType: "economic_intent", ResourceID: r.IntentID.String()}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedMarginLeveragedBuys200JSONResponse(res.Value), nil
}

// PostAdvancedMarginStressRuns runs every stress scenario against the
// caller's margin account and stores the report. It moves no value.
func (s *Server) PostAdvancedMarginStressRuns(ctx context.Context, request api.PostAdvancedMarginStressRunsRequestObject) (api.PostAdvancedMarginStressRunsResponseObject, error) {
	if s.opts.Ports.Margin == nil {
		return nil, errNotWired(marginFeature)
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	account, err := accountScopeWrite(ctx, request.Body.AccountId)
	if err != nil {
		return nil, err
	}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.MarginStressRun, commandMeta, error) {
			r, serr := s.opts.Ports.Margin.RunStress(ctx, account)
			if serr != nil {
				return api.MarginStressRun{}, commandMeta{}, serr
			}
			return toAPIMarginStressRun(r), commandMeta{Status: http.StatusOK, ResourceType: "margin_stress_run", ResourceID: r.ID.String()}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedMarginStressRuns200JSONResponse(res.Value), nil
}

// --- operator --------------------------------------------------------------------

// PostAdvancedMarginOpsFacilitiesSeed funds the lending pool and the
// insurance fund in sandbox dollars, exactly once per Idempotency-Key.
func (s *Server) PostAdvancedMarginOpsFacilitiesSeed(ctx context.Context, request api.PostAdvancedMarginOpsFacilitiesSeedRequestObject) (api.PostAdvancedMarginOpsFacilitiesSeedResponseObject, error) {
	if s.opts.Ports.Margin == nil {
		return nil, errNotWired(marginFeature)
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	pool, err := money.ParseQuantity(request.Body.PoolAmount)
	if err != nil {
		return nil, validationError("pool_amount", "pool_amount must be an integer string of sandbox-dollar base units")
	}
	fund, err := money.ParseQuantity(request.Body.FundAmount)
	if err != nil {
		return nil, validationError("fund_amount", "fund_amount must be an integer string of sandbox-dollar base units")
	}
	actor, err := marginActor(ctx)
	if err != nil {
		return nil, err
	}
	seed := MarginSeed{PoolAmount: pool, FundAmount: fund, Key: request.Params.IdempotencyKey, Actor: actor}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.MarginFacilities, commandMeta, error) {
			f, serr := s.opts.Ports.Margin.SeedFacilities(ctx, seed)
			if serr != nil {
				return api.MarginFacilities{}, commandMeta{}, serr
			}
			return toAPIMarginFacilities(f), commandMeta{Status: http.StatusOK, ResourceType: "facility", ResourceID: f.Pool.ID.String()}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedMarginOpsFacilitiesSeed200JSONResponse(res.Value), nil
}

// PostAdvancedMarginOpsMarkToMarket evaluates the open margin accounts and
// moves each between NORMAL, WARNING and MARGIN_CALL as the service decides.
func (s *Server) PostAdvancedMarginOpsMarkToMarket(ctx context.Context, request api.PostAdvancedMarginOpsMarkToMarketRequestObject) (api.PostAdvancedMarginOpsMarkToMarketResponseObject, error) {
	if s.opts.Ports.Margin == nil {
		return nil, errNotWired(marginFeature)
	}
	limit, err := marginBatchLimit(request.Body)
	if err != nil {
		return nil, err
	}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.MarginMarkToMarketResult, commandMeta, error) {
			v, merr := s.opts.Ports.Margin.MarkToMarket(ctx, limit)
			if merr != nil {
				return api.MarginMarkToMarketResult{}, commandMeta{}, merr
			}
			out, cerr := toAPIMarginMarkToMarket(ctx, v)
			if cerr != nil {
				return api.MarginMarkToMarketResult{}, commandMeta{}, cerr
			}
			return out, commandMeta{Status: http.StatusOK, ResourceType: "margin_mark_to_market"}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedMarginOpsMarkToMarket200JSONResponse(res.Value), nil
}

// GetAdvancedMarginOpsQueue lists the accounts a liquidation queue run
// considers, each with its live evaluation.
func (s *Server) GetAdvancedMarginOpsQueue(ctx context.Context, request api.GetAdvancedMarginOpsQueueRequestObject) (api.GetAdvancedMarginOpsQueueResponseObject, error) {
	if s.opts.Ports.Margin == nil {
		return nil, errNotWired(marginFeature)
	}
	entries, err := s.opts.Ports.Margin.Queue(ctx, pageLimit(request.Params.Limit, defaultPageLimit, maxPageLimit))
	if err != nil {
		return nil, err
	}
	out := api.MarginQueue{ValueDomain: api.SIMULATED, Items: make([]api.MarginQueueEntry, 0, len(entries))}
	for _, e := range entries {
		item, cerr := toAPIMarginQueueEntry(ctx, e)
		if cerr != nil {
			return nil, cerr
		}
		out.Items = append(out.Items, item)
	}
	return api.GetAdvancedMarginOpsQueue200JSONResponse(out), nil
}

// PostAdvancedMarginOpsQueueRun runs one liquidation step for each of the
// most under-margined accounts, as the service ranks them.
func (s *Server) PostAdvancedMarginOpsQueueRun(ctx context.Context, request api.PostAdvancedMarginOpsQueueRunRequestObject) (api.PostAdvancedMarginOpsQueueRunResponseObject, error) {
	if s.opts.Ports.Margin == nil {
		return nil, errNotWired(marginFeature)
	}
	limit, err := marginBatchLimit(request.Body)
	if err != nil {
		return nil, err
	}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.MarginQueueRunResult, commandMeta, error) {
			v, qerr := s.opts.Ports.Margin.RunQueue(ctx, limit)
			if qerr != nil {
				return api.MarginQueueRunResult{}, commandMeta{}, qerr
			}
			out := api.MarginQueueRunResult{ValueDomain: api.SIMULATED, Results: make([]api.MarginLiquidationResult, 0, len(v.Results)),
				Failures: make([]api.MarginFailure, 0, len(v.Failures))}
			for _, r := range v.Results {
				item, cerr := toAPIMarginLiquidation(r)
				if cerr != nil {
					return api.MarginQueueRunResult{}, commandMeta{}, cerr
				}
				out.Results = append(out.Results, item)
			}
			for _, f := range v.Failures {
				acct := f.AccountID
				out.Failures = append(out.Failures, toAPIMarginFailure(ctx, &acct, f.Err))
			}
			return out, commandMeta{Status: http.StatusOK, ResourceType: "margin_liquidation_queue"}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedMarginOpsQueueRun200JSONResponse(res.Value), nil
}

// PostAdvancedMarginOpsLiquidations executes at most one deterministic
// liquidation step of one account.
func (s *Server) PostAdvancedMarginOpsLiquidations(ctx context.Context, request api.PostAdvancedMarginOpsLiquidationsRequestObject) (api.PostAdvancedMarginOpsLiquidationsResponseObject, error) {
	if s.opts.Ports.Margin == nil {
		return nil, errNotWired(marginFeature)
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	// An operator acts on any account under sandbox:operate; there is no
	// ownership to check, only an identifier to read.
	account, err := accounts.ParseAccountID(request.Body.AccountId.String())
	if err != nil || account.IsZero() {
		return nil, validationError("account_id", "account_id must be a canonical UUID")
	}
	step := 0
	if request.Body.ExpectedStep != nil {
		if *request.Body.ExpectedStep < 0 {
			return nil, validationError("expected_step", "expected_step must not be negative")
		}
		step = *request.Body.ExpectedStep
	}
	req := margin.LiquidateRequest{AccountID: account, ExpectedStep: step}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.MarginLiquidationResult, commandMeta, error) {
			v, lerr := s.opts.Ports.Margin.Liquidate(ctx, req)
			if lerr != nil {
				return api.MarginLiquidationResult{}, commandMeta{}, lerr
			}
			out, cerr := toAPIMarginLiquidation(v)
			if cerr != nil {
				return api.MarginLiquidationResult{}, commandMeta{}, cerr
			}
			meta := commandMeta{Status: http.StatusOK, ResourceType: "margin_account", ResourceID: v.Result.MarginAccountID.String()}
			if v.Result.Case != nil {
				meta.ResourceType, meta.ResourceID = "margin_liquidation_case", v.Result.Case.ID.String()
			}
			return out, meta, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedMarginOpsLiquidations200JSONResponse(res.Value), nil
}

// PostAdvancedMarginOpsCasesMarginCaseIdCoverDeficit has the insurance fund
// cover a bankrupt case's deficit, exactly once per case.
func (s *Server) PostAdvancedMarginOpsCasesMarginCaseIdCoverDeficit(ctx context.Context, request api.PostAdvancedMarginOpsCasesMarginCaseIdCoverDeficitRequestObject) (api.PostAdvancedMarginOpsCasesMarginCaseIdCoverDeficitResponseObject, error) {
	if s.opts.Ports.Margin == nil {
		return nil, errNotWired(marginFeature)
	}
	cid, err := margin.ParseCaseID(request.MarginCaseId.String())
	if err != nil || cid.IsZero() {
		return nil, validationError("marginCaseId", "marginCaseId must be a canonical UUID")
	}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.MarginDeficitResult, commandMeta, error) {
			v, derr := s.opts.Ports.Margin.CoverDeficit(ctx, cid)
			if derr != nil {
				return api.MarginDeficitResult{}, commandMeta{}, derr
			}
			out := api.MarginDeficitResult{
				ValueDomain: api.SIMULATED, CaseId: toUUID(v.Result.CaseID), Deficit: v.Result.Deficit.String(),
				JournalTransactionId: toUUID(v.Result.JournalTransactionID), Replayed: v.Result.Replayed, Case: toAPIMarginCase(v.Case),
			}
			return out, commandMeta{Status: http.StatusOK, ResourceType: "margin_liquidation_case", ResourceID: cid.String()}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedMarginOpsCasesMarginCaseIdCoverDeficit200JSONResponse(res.Value), nil
}

// PostAdvancedMarginOpsStressRuns runs every stress scenario against any
// account's margin account and stores the report. It moves no value.
func (s *Server) PostAdvancedMarginOpsStressRuns(ctx context.Context, request api.PostAdvancedMarginOpsStressRunsRequestObject) (api.PostAdvancedMarginOpsStressRunsResponseObject, error) {
	if s.opts.Ports.Margin == nil {
		return nil, errNotWired(marginFeature)
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	account, err := accounts.ParseAccountID(request.Body.AccountId.String())
	if err != nil || account.IsZero() {
		return nil, validationError("account_id", "account_id must be a canonical UUID")
	}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.MarginStressRun, commandMeta, error) {
			r, serr := s.opts.Ports.Margin.RunStress(ctx, account)
			if serr != nil {
				return api.MarginStressRun{}, commandMeta{}, serr
			}
			return toAPIMarginStressRun(r), commandMeta{Status: http.StatusOK, ResourceType: "margin_stress_run", ResourceID: r.ID.String()}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedMarginOpsStressRuns200JSONResponse(res.Value), nil
}

// GetAdvancedMarginOpsCalls pages margin calls across every account.
func (s *Server) GetAdvancedMarginOpsCalls(ctx context.Context, request api.GetAdvancedMarginOpsCallsRequestObject) (api.GetAdvancedMarginOpsCallsResponseObject, error) {
	if s.opts.Ports.Margin == nil {
		return nil, errNotWired(marginFeature)
	}
	var status margin.CallStatus
	if request.Params.Status != nil {
		status = margin.CallStatus(*request.Params.Status)
		known := false
		for _, st := range margin.AllCallStatuses() {
			known = known || st == status
		}
		if !known {
			return nil, validationError("status", "status must be OPEN, MET or LIQUIDATED")
		}
	}
	page, err := s.opts.Ports.Margin.AllCalls(ctx, status, marginCursorParam(request.Params.Cursor),
		pageLimit(request.Params.Limit, defaultPageLimit, maxPageLimit))
	if err != nil {
		return nil, err
	}
	return api.GetAdvancedMarginOpsCalls200JSONResponse(toAPIMarginCallPage(page)), nil
}

// --- request helpers ---------------------------------------------------------------

func marginCursorParam(c *api.Cursor) string {
	if c == nil {
		return ""
	}
	return *c
}

func marginPrincipal(ctx context.Context) (security.Principal, error) {
	p, ok := security.PrincipalFrom(ctx)
	if !ok || p.SubjectID == "" {
		return security.Principal{}, errs.New(errs.CodeUnauthenticated, "authentication is required")
	}
	return p, nil
}

func marginActor(ctx context.Context) (econ.Actor, error) {
	p, err := marginPrincipal(ctx)
	if err != nil {
		return econ.Actor{}, err
	}
	return econ.Actor{Type: p.ActorType, ID: p.SubjectID}, nil
}

func marginRequester(ctx context.Context) (margin.Requester, error) {
	p, err := marginPrincipal(ctx)
	if err != nil {
		return margin.Requester{}, err
	}
	return margin.Requester{Type: p.ActorType, ID: p.SubjectID}, nil
}

func marginCollateralRequest(ctx context.Context, account accounts.AccountID, body api.MarginCollateralRequest, key string) (margin.CollateralRequest, error) {
	asset, err := assets.ParseAssetID(body.AssetId.String())
	if err != nil || asset.IsZero() {
		return margin.CollateralRequest{}, validationError("asset_id", "asset_id must be a canonical UUID")
	}
	qty, err := money.ParseQuantity(body.Quantity)
	if err != nil {
		return margin.CollateralRequest{}, validationError("quantity", "quantity must be an integer string of the asset's base units")
	}
	requester, err := marginRequester(ctx)
	if err != nil {
		return margin.CollateralRequest{}, err
	}
	return margin.CollateralRequest{AccountID: account, AssetID: asset, Quantity: qty, IdempotencyKey: key, Requester: requester,
		CorrelationID: observability.CorrelationID(ctx)}, nil
}

func marginLoanRequest(ctx context.Context, account accounts.AccountID, body api.MarginAmountRequest, key string) (margin.LoanRequest, error) {
	amount, err := money.ParseQuantity(body.Amount)
	if err != nil {
		return margin.LoanRequest{}, validationError("amount", "amount must be an integer string of numeraire base units")
	}
	requester, err := marginRequester(ctx)
	if err != nil {
		return margin.LoanRequest{}, err
	}
	return margin.LoanRequest{AccountID: account, Amount: amount, IdempotencyKey: key, Requester: requester,
		CorrelationID: observability.CorrelationID(ctx)}, nil
}

// marginBatchLimit reads an operator batch's optional limit. Absent means the
// service's own bound; the service clamps any limit to its maximum.
func marginBatchLimit(body *api.MarginBatchRequest) (int, error) {
	if body == nil || body.Limit == nil {
		return 0, nil
	}
	if *body.Limit < 1 {
		return 0, validationError("limit", "limit must be at least 1")
	}
	return *body.Limit, nil
}

// --- response shapes ----------------------------------------------------------------

// marginUUID renders an identifier the margin engine carries as text. The
// engine only ever writes canonical UUIDs; anything else is an internal fault,
// never a fabricated id.
func marginUUID(s string) (api.UUID, error) {
	u, err := uuid.Parse(s)
	if err != nil {
		return api.UUID{}, errs.New(errs.CodeInternal, "the margin engine reported an identifier that is not a UUID")
	}
	return u, nil
}

func toAPIMarginPrice(p *money.Price) *api.MarginPrice {
	if p == nil {
		return nil
	}
	out := api.MarginPrice{Mantissa: p.Mantissa.String(), Scale: int(p.Scale), Source: strPtr(p.Source)}
	if !p.At.IsZero() {
		at := p.At.UTC()
		out.ObservedAt = &at
	}
	return &out
}

func toAPIMarginAccount(a margin.Account) api.MarginAccount {
	out := api.MarginAccount{
		ValueDomain: api.SIMULATED, Id: toUUID(a.ID), AccountId: toUUID(a.AccountID), MarginMode: api.MarginMode(a.MarginMode),
		Mode: api.AdvancedMode(a.Mode), Status: api.MarginAccountStatus(a.Status), PolicyVersion: a.PolicyVersion,
		NumeraireAssetId: toUUID(a.NumeraireAssetID), LendingPoolId: toUUID(a.LendingPoolID), InsuranceFundId: toUUID(a.InsuranceFundID),
		OpenedAt: a.OpenedAt.UTC(),
	}
	if a.IsolatedAssetID != nil {
		out.IsolatedAssetId = uuidPtr(*a.IsolatedAssetID)
	}
	return out
}

// marginMarks indexes the marks an evaluation saw by asset id.
func marginMarks(st margin.AccountState) map[string]*money.Price {
	out := map[string]*money.Price{}
	for _, h := range st.Holdings {
		if h.Mark != nil {
			m := *h.Mark
			out[h.AssetID] = &m
		}
	}
	return out
}

// toAPIMarginEvaluation renders the engine's evaluation. Quantities are the
// engine's exact numeraire base units; the usd block is the engine's own
// conservative cent rendering (margin.Evaluation.USD). marks may be nil.
func toAPIMarginEvaluation(ev margin.Evaluation, marks map[string]*money.Price) (api.MarginEvaluation, error) {
	u, err := ev.USD()
	if err != nil {
		return api.MarginEvaluation{}, err
	}
	numeraire, err := marginUUID(ev.NumeraireAssetID)
	if err != nil {
		return api.MarginEvaluation{}, err
	}
	out := api.MarginEvaluation{
		At: ev.At.UTC(), PolicyVersion: ev.PolicyVersion, PolicyHash: ev.PolicyHash, Hash: ev.Hash,
		NumeraireAssetId: numeraire, NumeraireDecimals: int(ev.NumeraireDecimals), Health: api.MarginAccountStatus(ev.Health),
		Levered: ev.Levered, LiquidationEligible: ev.LiquidationEligible, PricesFresh: ev.PricesFresh, MissingMarks: ev.MissingMarks,
		StaleAssets: make([]api.UUID, 0, len(ev.StaleAssets)), OldestMarkAt: timePtr(ev.OldestMarkAt), NewestMarkAt: timePtr(ev.NewestMarkAt),
		LeverageCapBps: int64(ev.LeverageCapBPS), EffectiveLeverageBps: int64(ev.EffectiveLeverageBPS), LeverageUnbounded: ev.LeverageUnbounded,
		EligibleCollateral: ev.EligibleCollateral.String(), RealizedPnl: ev.RealizedPnL.String(), UnrealizedPnl: ev.UnrealizedPnL.String(),
		BorrowLiabilities: ev.BorrowLiabilities.String(), AccruedFees: ev.AccruedFees.String(), AccruedInterest: ev.AccruedInterest.String(),
		Equity: ev.Equity.String(), MarketValue: ev.MarketValue.String(), NetAssetValue: ev.NetAssetValue.String(),
		GrossExposure: ev.GrossExposure.String(), NetExposure: ev.NetExposure.String(), InitialMargin: ev.InitialMargin.String(),
		MaintenanceMargin: ev.MaintenanceMargin.String(), WarningThreshold: ev.WarningThreshold.String(),
		LiquidationBuffer: ev.LiquidationBuffer.String(), Capacity: ev.Capacity.String(), MarginAvailable: ev.MarginAvailable.String(),
		Usd: api.MarginEvaluationUSD{
			EligibleCollateral: u.EligibleCollateral.String(), BorrowLiabilities: u.BorrowLiabilities.String(),
			AccruedInterest: u.AccruedInterest.String(), Equity: u.Equity.String(), MarketValue: u.MarketValue.String(),
			GrossExposure: u.GrossExposure.String(), InitialMargin: u.InitialMargin.String(), MaintenanceMargin: u.MaintenanceMargin.String(),
			WarningThreshold: u.WarningThreshold.String(), MarginAvailable: u.MarginAvailable.String(),
			LiquidationBuffer: u.LiquidationBuffer.String(),
		},
		Holdings: make([]api.MarginHolding, 0, len(ev.Holdings)),
	}
	for _, s := range ev.StaleAssets {
		id, err := marginUUID(s)
		if err != nil {
			return api.MarginEvaluation{}, err
		}
		out.StaleAssets = append(out.StaleAssets, id)
	}
	for _, h := range ev.Holdings {
		id, err := marginUUID(h.AssetID)
		if err != nil {
			return api.MarginEvaluation{}, err
		}
		hc := h.Haircut
		out.Holdings = append(out.Holdings, api.MarginHolding{
			AssetId: id, Numeraire: h.Numeraire, Quantity: h.Quantity.String(), MarketValue: h.MarketValue.String(),
			Priced: h.Priced, Fresh: h.Fresh, MarkAt: timePtr(h.MarkAt), Mark: toAPIMarginPrice(marks[h.AssetID]),
			ConcentrationShareBps: int(h.ConcentrationShareBPS),
			Haircut: api.MarginHaircut{
				VolatilityBps: int(hc.VolatilityBPS), LiquidityBps: int(hc.LiquidityBPS), OracleBps: int(hc.OracleBPS),
				VenueBps: int(hc.VenueBPS), ConcentrationBps: int(hc.ConcentrationBPS), SettlementBps: int(hc.SettlementBPS),
				CrossChainBps: int(hc.CrossChainBPS), ZeroValue: hc.ZeroValue, TotalBps: int(hc.TotalBPS),
			},
			CollateralValue: h.CollateralValue.String(), Exposure: h.Exposure.String(), InitialMargin: h.InitialMargin.String(),
			MaintenanceMargin: h.MaintenanceMargin.String(),
		})
	}
	return out, nil
}

// toAPIMarginResultEvaluation renders the evaluation a command result
// carries. A result the service replayed carries none (it evaluated
// nothing), and none is shown rather than a zero one.
func toAPIMarginResultEvaluation(replayed bool, ev margin.Evaluation) (*api.MarginEvaluation, error) {
	if replayed || ev.Hash == "" {
		return nil, nil
	}
	out, err := toAPIMarginEvaluation(ev, nil)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func toAPIMarginCall(v MarginCallView) api.MarginCall {
	c := v.Call
	return api.MarginCall{
		Id: toUUID(c.ID), MarginAccountId: toUUID(c.MarginAccountID), AccountId: toUUID(v.Facts.AccountID),
		Status: api.MarginCallStatus(c.Status), IssuedAt: c.IssuedAt.UTC(), Equity: c.EquityUnits.String(),
		MaintenanceMargin: c.MaintenanceUnits.String(), Deficiency: c.DeficiencyUnits.String(), EvaluationHash: c.EvaluationHash,
		ResolvedAt: timePtr(c.ResolvedAt), NumeraireAssetId: toUUID(v.Facts.NumeraireAssetID),
		NumeraireDecimals: int(v.Facts.NumeraireDecimals),
	}
}

func toAPIMarginCallPage(page MarginCallPage) api.MarginCallPage {
	out := api.MarginCallPage{ValueDomain: api.SIMULATED, Items: make([]api.MarginCall, 0, len(page.Items)), NextCursor: nextCursor(page.NextCursor)}
	for _, c := range page.Items {
		out.Items = append(out.Items, toAPIMarginCall(c))
	}
	return out
}

func toAPIMarginCase(v MarginCaseView) api.MarginCase {
	c := v.Case
	out := api.MarginCase{
		Id: toUUID(c.ID), MarginAccountId: toUUID(c.MarginAccountID), AccountId: toUUID(v.Facts.AccountID),
		MarginCallId: toUUID(c.MarginCallID), Status: api.MarginCaseStatus(c.Status), PolicyVersion: c.PolicyVersion,
		Target: api.MarginLiquidationTarget(c.Target), MaxSteps: c.MaxSteps, StepsExecuted: c.StepsExecuted, OpenedAt: c.OpenedAt.UTC(),
		ClosedAt: timePtr(c.ClosedAt), LastReason: c.LastReason, Deficit: qtyPtr(c.DeficitUnits), DeficitCoveredAt: timePtr(c.CoveredAt),
		NumeraireAssetId: toUUID(v.Facts.NumeraireAssetID), NumeraireDecimals: int(v.Facts.NumeraireDecimals),
	}
	if c.CoverageJournalID != nil {
		out.DeficitCoverageJournalTransactionId = uuidPtr(*c.CoverageJournalID)
	}
	return out
}

func toAPIMarginStep(s margin.Step) api.MarginLiquidationStep {
	out := api.MarginLiquidationStep{
		Id: toUUID(s.ID), StepNo: s.StepNo, Action: api.MarginStepAction(s.Action), AssetId: toUUID(s.AssetID),
		Quantity: s.Quantity.String(), Mark: toAPIMarginPrice(s.Mark), ExecutionPrice: toAPIMarginPrice(s.ExecutionPrice),
		SlippageBps: int(s.SlippageBPS), SlippageCapBps: int(s.SlippageCapBPS), Proceeds: s.Proceeds.String(), Fee: s.Fee.String(),
		Repaid: s.Repaid.String(), Excess: s.Excess.String(), EquityBefore: s.EquityBefore.String(), EquityAfter: s.EquityAfter.String(),
		PlanHash: s.PlanHash, ExecutedAt: s.ExecutedAt.UTC(),
	}
	if s.TradeJournalID != nil {
		out.TradeJournalTransactionId = uuidPtr(*s.TradeJournalID)
	}
	if s.ReleaseJournal != nil {
		out.ReleaseJournalTransactionId = uuidPtr(*s.ReleaseJournal)
	}
	if s.RepayJournalID != nil {
		out.RepayJournalTransactionId = uuidPtr(*s.RepayJournalID)
	}
	if s.ExcessJournalID != nil {
		out.ExcessJournalTransactionId = uuidPtr(*s.ExcessJournalID)
	}
	return out
}

func toAPIMarginCaseDetail(v MarginCaseView) api.MarginCaseDetail {
	c := toAPIMarginCase(v)
	out := api.MarginCaseDetail{
		ValueDomain: api.SIMULATED, Id: c.Id, MarginAccountId: c.MarginAccountId, AccountId: c.AccountId, MarginCallId: c.MarginCallId,
		Status: c.Status, PolicyVersion: c.PolicyVersion, Target: c.Target, MaxSteps: c.MaxSteps, StepsExecuted: c.StepsExecuted,
		OpenedAt: c.OpenedAt, ClosedAt: c.ClosedAt, LastReason: c.LastReason, Deficit: c.Deficit,
		DeficitCoverageJournalTransactionId: c.DeficitCoverageJournalTransactionId, DeficitCoveredAt: c.DeficitCoveredAt,
		NumeraireAssetId: c.NumeraireAssetId, NumeraireDecimals: c.NumeraireDecimals,
		Steps: make([]api.MarginLiquidationStep, 0, len(v.Steps)),
	}
	for _, s := range v.Steps {
		out.Steps = append(out.Steps, toAPIMarginStep(s))
	}
	return out
}

func toAPIMarginAccountDetail(v MarginAccountView) (api.MarginAccountDetail, error) {
	ev, err := toAPIMarginEvaluation(v.Evaluation, marginMarks(v.State))
	if err != nil {
		return api.MarginAccountDetail{}, err
	}
	a := toAPIMarginAccount(v.Account)
	out := api.MarginAccountDetail{
		ValueDomain: a.ValueDomain, Id: a.Id, AccountId: a.AccountId, MarginMode: a.MarginMode, Mode: a.Mode, Status: a.Status,
		PolicyVersion: a.PolicyVersion, NumeraireAssetId: a.NumeraireAssetId, IsolatedAssetId: a.IsolatedAssetId,
		LendingPoolId: a.LendingPoolId, InsuranceFundId: a.InsuranceFundId, OpenedAt: a.OpenedAt,
		Evaluation: ev, Loans: make([]api.MarginLoan, 0, len(v.State.Loans)),
	}
	for _, l := range v.State.Loans {
		lot, err := marginUUID(l.LotID)
		if err != nil {
			return api.MarginAccountDetail{}, err
		}
		out.Loans = append(out.Loans, api.MarginLoan{LotId: lot, Principal: l.Principal.String(), Interest: l.Interest.String(),
			RateBps: int64(l.RateBPS), AccruedThrough: l.AccruedThrough.UTC()})
	}
	if v.OpenCall != nil {
		c := toAPIMarginCall(*v.OpenCall)
		out.OpenCall = &c
	}
	if v.LatestCase != nil {
		c := toAPIMarginCase(*v.LatestCase)
		out.LatestCase = &c
	}
	return out, nil
}

func toAPIMarginStressRunSummary(v MarginStressRunView) api.MarginStressRunSummary {
	return api.MarginStressRunSummary{
		Id: toUUID(v.ID), MarginAccountId: toUUID(v.MarginAccountID), AccountId: toUUID(v.Facts.AccountID), RunAt: v.RunAt.UTC(),
		PolicyVersion: v.Report.PolicyVersion, PolicyHash: v.Report.PolicyHash, InputHash: v.Report.InputHash, ReportHash: v.Report.Hash,
		NumeraireAssetId: toUUID(v.Facts.NumeraireAssetID), NumeraireDecimals: int(v.Facts.NumeraireDecimals),
	}
}

func toAPIMarginStressRun(v MarginStressRunView) api.MarginStressRun {
	s := toAPIMarginStressRunSummary(v)
	out := api.MarginStressRun{
		ValueDomain: api.SIMULATED, Id: s.Id, MarginAccountId: s.MarginAccountId, AccountId: s.AccountId, RunAt: s.RunAt,
		PolicyVersion: s.PolicyVersion, PolicyHash: s.PolicyHash, InputHash: s.InputHash, ReportHash: s.ReportHash,
		NumeraireAssetId: s.NumeraireAssetId, NumeraireDecimals: s.NumeraireDecimals,
		Results: make([]api.MarginStressResult, 0, len(v.Report.Results)),
	}
	for _, r := range v.Report.Results {
		out.Results = append(out.Results, api.MarginStressResult{
			Scenario: api.MarginStressScenario(r.Scenario), ShockBps: int(r.ShockBPS), EquityAfter: r.EquityAfter.String(),
			MaintenanceAfter: r.MaintenanceAfter.String(), InitialAfter: r.InitialAfter.String(),
			EquityAfterUsd: r.EquityAfterUSD.String(), MaintenanceAfterUsd: r.MaintenanceAftUSD.String(),
			WouldLiquidate: r.WouldLiquidate, Executable: r.Executable, Outcome: api.MarginPlanDecision(r.Outcome),
			StepsSimulated: r.StepsSimulated, Deficit: r.Deficit.String(), DeficitUsd: r.DeficitUSD.String(),
			InsuranceFundDraw: r.InsuranceFundDraw.String(), InsuranceFundDrawUsd: r.InsuranceFundDrawUSD.String(),
		})
	}
	return out
}

func toAPIMarginCollateralResult(r margin.CollateralResult) (api.MarginCollateralResult, error) {
	ev, err := toAPIMarginResultEvaluation(r.Replayed, r.Evaluation)
	if err != nil {
		return api.MarginCollateralResult{}, err
	}
	out := api.MarginCollateralResult{
		ValueDomain: api.SIMULATED, MarginAccountId: toUUID(r.MarginAccountID), MoveId: toUUID(r.MoveID),
		JournalTransactionId: toUUID(r.JournalTransactionID), EffectId: toUUID(r.EffectID), ReservationId: parseUUIDText(r.ReservationID),
		Replayed: r.Replayed, Status: api.MarginAccountStatus(r.Status), Evaluation: ev,
	}
	if r.IntentID != nil {
		out.IntentId = uuidPtr(*r.IntentID)
	}
	return out, nil
}

func toAPIMarginLoanResult(r margin.LoanResult) (api.MarginLoanResult, error) {
	ev, err := toAPIMarginResultEvaluation(r.Replayed, r.Evaluation)
	if err != nil {
		return api.MarginLoanResult{}, err
	}
	out := api.MarginLoanResult{
		ValueDomain: api.SIMULATED, MarginAccountId: toUUID(r.MarginAccountID), IntentId: toUUID(r.IntentID),
		JournalTransactionId: toUUID(r.JournalTransactionID), EffectId: toUUID(r.EffectID),
		Allocations: make([]api.MarginLoanAllocation, 0, len(r.Allocations)), Replayed: r.Replayed,
		Status: api.MarginAccountStatus(r.Status), Evaluation: ev,
	}
	if r.LotID != nil {
		out.LotId = uuidPtr(*r.LotID)
	}
	for _, al := range r.Allocations {
		lot, err := marginUUID(al.LotID)
		if err != nil {
			return api.MarginLoanResult{}, err
		}
		out.Allocations = append(out.Allocations, api.MarginLoanAllocation{LotId: lot, Interest: al.Interest.String(), Principal: al.Principal.String()})
	}
	return out, nil
}

func toAPIMarginLeveragedBuyResult(r margin.SpotResult) (api.MarginLeveragedBuyResult, error) {
	ev, err := toAPIMarginResultEvaluation(r.Replayed, r.Evaluation)
	if err != nil {
		return api.MarginLeveragedBuyResult{}, err
	}
	out := api.MarginLeveragedBuyResult{
		ValueDomain: api.SIMULATED, MarginAccountId: toUUID(r.MarginAccountID), IntentId: toUUID(r.IntentID), Cost: r.Cost.String(),
		BorrowJournalTransactionId: toUUID(r.BorrowJournal), TradeJournalTransactionId: toUUID(r.TradeJournal),
		CollateralJournalTransactionId: toUUID(r.CollateralJrnl), ReservationId: parseUUIDText(r.ReservationID),
		Replayed: r.Replayed, Status: api.MarginAccountStatus(r.Status), Evaluation: ev,
	}
	if r.LotID != nil {
		out.LotId = uuidPtr(*r.LotID)
	}
	// A replayed buy reports what the books hold (the journals, the lot and
	// its cost) and nothing the service did not recompute: no mark, price or
	// slippage is shown for it.
	if !r.Replayed {
		mark, exec := r.Mark, r.ExecutionPrice
		out.Mark, out.ExecutionPrice = toAPIMarginPrice(&mark), toAPIMarginPrice(&exec)
		slip := int(r.SlippageBPS)
		out.SlippageBps = &slip
	}
	return out, nil
}

func toAPIMarginFacility(f econ.Facility, cash money.Quantity) api.MarginFacility {
	return api.MarginFacility{Id: toUUID(f.ID), Kind: string(f.Kind), Name: f.Name, Status: string(f.Status), Cash: cash.String()}
}

func toAPIMarginFacilities(v MarginFacilitiesView) api.MarginFacilities {
	return api.MarginFacilities{
		ValueDomain: api.SIMULATED, AssetId: toUUID(v.AssetID), AssetDecimals: int(v.AssetDecimals),
		LendingPool: toAPIMarginFacility(v.Pool, v.PoolCash), InsuranceFund: toAPIMarginFacility(v.Fund, v.FundCash),
	}
}

// toAPIMarginFailure renders a per-account failure with the same redaction a
// problem document gets: an internal error says only that it was internal.
func toAPIMarginFailure(ctx context.Context, account *accounts.AccountID, err error) api.MarginFailure {
	p := errs.ToProblem(classify(ctx, err), "", "")
	out := api.MarginFailure{Code: string(p.Code), Detail: p.Detail}
	if account != nil {
		out.AccountId = uuidPtr(*account)
	}
	return out
}

func toAPIMarginMarkToMarket(ctx context.Context, v MarginMarkView) (api.MarginMarkToMarketResult, error) {
	out := api.MarginMarkToMarketResult{ValueDomain: api.SIMULATED, Evaluated: make([]api.MarginEvaluated, 0, len(v.Evaluated)),
		Failures: make([]api.MarginFailure, 0, len(v.Failures))}
	for _, r := range v.Evaluated {
		ev, err := toAPIMarginEvaluation(r.Evaluation, nil)
		if err != nil {
			return api.MarginMarkToMarketResult{}, err
		}
		item := api.MarginEvaluated{
			MarginAccountId: toUUID(r.MarginAccountID), AccountId: toUUID(r.AccountID), PreviousStatus: api.MarginAccountStatus(r.Previous),
			Status: api.MarginAccountStatus(r.Status), Evaluation: ev,
		}
		if r.Call != nil {
			numeraire, err := assets.ParseAssetID(r.Evaluation.NumeraireAssetID)
			if err != nil {
				return api.MarginMarkToMarketResult{}, errs.New(errs.CodeInternal, "the margin engine reported an identifier that is not a UUID")
			}
			c := toAPIMarginCall(MarginCallView{Call: *r.Call, Facts: MarginFacts{AccountID: r.AccountID, NumeraireAssetID: numeraire,
				NumeraireDecimals: r.Evaluation.NumeraireDecimals}})
			item.Call = &c
		}
		out.Evaluated = append(out.Evaluated, item)
	}
	for _, f := range v.Failures {
		out.Failures = append(out.Failures, toAPIMarginFailure(ctx, nil, f))
	}
	return out, nil
}

func toAPIMarginQueueEntry(ctx context.Context, e MarginQueueEntry) (api.MarginQueueEntry, error) {
	out := api.MarginQueueEntry{Account: toAPIMarginAccount(e.Account), NumeraireDecimals: int(e.Facts.NumeraireDecimals)}
	if e.Evaluation != nil {
		var marks map[string]*money.Price
		if e.State != nil {
			marks = marginMarks(*e.State)
		}
		ev, err := toAPIMarginEvaluation(*e.Evaluation, marks)
		if err != nil {
			return api.MarginQueueEntry{}, err
		}
		out.Evaluation = &ev
	}
	if e.EvaluationErr != nil {
		acct := e.Account.AccountID
		f := toAPIMarginFailure(ctx, &acct, e.EvaluationErr)
		out.EvaluationError = &f
	}
	if e.OpenCall != nil {
		c := toAPIMarginCall(*e.OpenCall)
		out.OpenCall = &c
	}
	if e.LatestCase != nil {
		c := toAPIMarginCase(*e.LatestCase)
		out.LatestCase = &c
	}
	return out, nil
}

func toAPIMarginPlan(p margin.Plan) (api.MarginPlan, error) {
	out := api.MarginPlan{
		Decision: api.MarginPlanDecision(p.Decision), Reason: p.Reason, Target: api.MarginLiquidationTarget(p.Target),
		StepsTaken: p.StepsTaken, TargetUnits: p.TargetUnits.String(), DeficitUnits: p.DeficitUnits.String(), Hash: p.Hash,
		Orders: make([]api.MarginLiquidationOrder, 0, len(p.Orders)),
	}
	for _, o := range p.Orders {
		asset, err := marginUUID(o.AssetID)
		if err != nil {
			return api.MarginPlan{}, err
		}
		out.Orders = append(out.Orders, api.MarginLiquidationOrder{
			Action: api.MarginStepAction(o.Action), AssetId: asset, Quantity: o.Quantity.String(), Mark: toAPIMarginPrice(o.Mark),
			ExecutionPrice: toAPIMarginPrice(o.ExecutionPrice), SlippageBps: int(o.SlippageBPS), Proceeds: o.Proceeds.String(),
			Fee: o.Fee.String(), Repay: o.Repay.String(), Excess: o.Excess.String(),
		})
	}
	return out, nil
}

func toAPIMarginLiquidation(v MarginLiquidationView) (api.MarginLiquidationResult, error) {
	r := v.Result
	out := api.MarginLiquidationResult{
		ValueDomain: api.SIMULATED, MarginAccountId: toUUID(r.MarginAccountID), AccountId: toUUID(v.Facts.AccountID),
		Eligible: r.Eligible, Decision: api.MarginPlanDecision(r.Decision), Reason: r.Reason,
		AccountStatus: api.MarginAccountStatus(r.AccountStatus), NumeraireDecimals: int(v.Facts.NumeraireDecimals), Replayed: r.Replayed,
	}
	if r.Case != nil {
		c := toAPIMarginCase(MarginCaseView{Case: *r.Case, Facts: v.Facts})
		out.Case = &c
	}
	if r.Step != nil {
		s := toAPIMarginStep(*r.Step)
		out.Step = &s
	}
	if r.Plan != nil {
		p, err := toAPIMarginPlan(*r.Plan)
		if err != nil {
			return api.MarginLiquidationResult{}, err
		}
		out.Plan = &p
	}
	return out, nil
}
