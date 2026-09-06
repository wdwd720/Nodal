package httpapi

import (
	"bytes"
	"context"
	"io"
	"time"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/capital/buyingpower"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gen/api"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/security"
)

const (
	defaultPageLimit = 50
	maxPageLimit     = 200
)

// accountScope resolves the path account id and enforces tenant scoping. Every
// account-scoped handler starts here: an operator needs account:read_any, a
// customer must own the account, and an agent never gets this far.
func accountScope(ctx context.Context, u api.AccountId) (accounts.AccountID, error) {
	var zero accounts.AccountID
	accountID, err := accounts.ParseAccountID(u.String())
	if err != nil || accountID.IsZero() {
		return zero, validationError("accountId", "accountId must be a canonical UUID")
	}
	if err := security.RequireAccount(ctx, accountID.String()); err != nil {
		return zero, err
	}
	return accountID, nil
}

// GetAccounts lists the accounts the caller owns.
func (s *Server) GetAccounts(ctx context.Context, _ api.GetAccountsRequestObject) (api.GetAccountsResponseObject, error) {
	if s.opts.Ports.Accounts == nil {
		return nil, errNotWired("accounts")
	}
	p, ok := security.PrincipalFrom(ctx)
	if !ok {
		return nil, errs.New(errs.CodeUnauthenticated, "authentication is required")
	}
	owner, err := accounts.ParseUserID(p.SubjectID)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeUnauthenticated, "the session subject is not a platform user")
	}
	list, err := s.opts.Ports.Accounts.ListByOwner(ctx, owner)
	if err != nil {
		return nil, err
	}
	return api.GetAccounts200JSONResponse(toAPIAccounts(list)), nil
}

// GetAccountsAccountId returns one account.
func (s *Server) GetAccountsAccountId(ctx context.Context, request api.GetAccountsAccountIdRequestObject) (api.GetAccountsAccountIdResponseObject, error) {
	if s.opts.Ports.Accounts == nil {
		return nil, errNotWired("accounts")
	}
	accountID, err := accountScope(ctx, request.AccountId)
	if err != nil {
		return nil, err
	}
	a, err := s.opts.Ports.Accounts.Get(ctx, accountID)
	if err != nil {
		return nil, err
	}
	return api.GetAccountsAccountId200JSONResponse(toAPIAccount(a)), nil
}

// GetAccountsAccountIdBuyingPower recomputes buying power now (PART 25). The
// figure is never cached, and the purpose only changes which restrictions are
// reported blocking, never the arithmetic.
func (s *Server) GetAccountsAccountIdBuyingPower(ctx context.Context, request api.GetAccountsAccountIdBuyingPowerRequestObject) (api.GetAccountsAccountIdBuyingPowerResponseObject, error) {
	if s.opts.Ports.BuyingPower == nil {
		return nil, errNotWired("buying power")
	}
	accountID, err := accountScope(ctx, request.AccountId)
	if err != nil {
		return nil, err
	}
	purpose := buyingpower.PurposeDisplay
	if request.Params.Purpose != nil {
		purpose = buyingpower.Purpose(*request.Params.Purpose)
		if !purpose.Valid() {
			return nil, validationError("purpose", "unknown purpose")
		}
	}
	bp, err := s.opts.Ports.BuyingPower.Compute(ctx, accountID, purpose)
	if err != nil {
		return nil, err
	}
	return api.GetAccountsAccountIdBuyingPower200JSONResponse(toAPIBuyingPower(bp)), nil
}

// GetAccountsAccountIdHoldings returns the portfolio view.
func (s *Server) GetAccountsAccountIdHoldings(ctx context.Context, request api.GetAccountsAccountIdHoldingsRequestObject) (api.GetAccountsAccountIdHoldingsResponseObject, error) {
	if s.opts.Ports.Holdings == nil {
		return nil, errNotWired("holdings")
	}
	accountID, err := accountScope(ctx, request.AccountId)
	if err != nil {
		return nil, err
	}
	h, err := s.opts.Ports.Holdings.Holdings(ctx, accountID)
	if err != nil {
		return nil, err
	}
	return api.GetAccountsAccountIdHoldings200JSONResponse(toAPIHoldings(h)), nil
}

// GetAccountsAccountIdLedgerTransactions pages the account's append-only
// journal history, newest first.
func (s *Server) GetAccountsAccountIdLedgerTransactions(ctx context.Context, request api.GetAccountsAccountIdLedgerTransactionsRequestObject) (api.GetAccountsAccountIdLedgerTransactionsResponseObject, error) {
	if s.opts.Ports.Ledger == nil {
		return nil, errNotWired("the ledger")
	}
	accountID, err := accountScope(ctx, request.AccountId)
	if err != nil {
		return nil, err
	}
	cursor := ""
	if request.Params.Cursor != nil {
		cursor = *request.Params.Cursor
	}
	limit := pageLimit(request.Params.Limit, defaultPageLimit, maxPageLimit)
	txs, next, err := s.opts.Ports.Ledger.ListTransactions(ctx, ledger.TransactionFilter{
		OwnerType: ledger.OwnerCustomer,
		OwnerID:   accountID.String(),
	}, cursor, limit)
	if err != nil {
		return nil, err
	}
	items := make([]api.JournalTransaction, 0, len(txs))
	for _, t := range txs {
		items = append(items, toAPIJournalTransaction(t))
	}
	return api.GetAccountsAccountIdLedgerTransactions200JSONResponse(api.JournalTransactionPage{
		Items:      items,
		NextCursor: nextCursor(next),
	}), nil
}

// GetAccountsAccountIdActivity pages the account's activity timeline.
func (s *Server) GetAccountsAccountIdActivity(ctx context.Context, request api.GetAccountsAccountIdActivityRequestObject) (api.GetAccountsAccountIdActivityResponseObject, error) {
	if s.opts.Ports.Activity == nil {
		return nil, errNotWired("the activity timeline")
	}
	accountID, err := accountScope(ctx, request.AccountId)
	if err != nil {
		return nil, err
	}
	cursor := ""
	if request.Params.Cursor != nil {
		cursor = *request.Params.Cursor
	}
	limit := pageLimit(request.Params.Limit, defaultPageLimit, maxPageLimit)
	page, err := s.opts.Ports.Activity.Activity(ctx, accountID, cursor, limit)
	if err != nil {
		return nil, err
	}
	return api.GetAccountsAccountIdActivity200JSONResponse(api.ActivityPage{
		Items:      toAPIActivity(page.Items),
		NextCursor: nextCursor(page.NextCursor),
	}), nil
}

// GetAccountsAccountIdExport renders the account export (PART 202).
func (s *Server) GetAccountsAccountIdExport(ctx context.Context, request api.GetAccountsAccountIdExportRequestObject) (api.GetAccountsAccountIdExportResponseObject, error) {
	if s.opts.Ports.Export == nil {
		return nil, errNotWired("account export")
	}
	accountID, err := accountScope(ctx, request.AccountId)
	if err != nil {
		return nil, err
	}
	var from, to time.Time
	if request.Params.From != nil {
		from = request.Params.From.UTC()
	}
	if request.Params.To != nil {
		to = request.Params.To.UTC()
	}
	if !from.IsZero() && !to.IsZero() && to.Before(from) {
		return nil, validationError("to", "to must not precede from")
	}
	doc, err := s.opts.Ports.Export.Export(ctx, accountID, from, to)
	if err != nil {
		return nil, err
	}
	if request.Params.Format != nil && *request.Params.Format == "csv" {
		return api.GetAccountsAccountIdExport200TextcsvResponse{
			Body:          io.NopCloser(bytes.NewReader(doc.CSV)),
			ContentLength: int64(len(doc.CSV)),
		}, nil
	}
	return api.GetAccountsAccountIdExport200JSONResponse(doc.JSON), nil
}
