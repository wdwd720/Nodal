package httpapi

import (
	"context"
	"net/http"

	"github.com/google/uuid"

	"github.com/nodal/controlplane/internal/commerce"
	"github.com/nodal/controlplane/internal/errs"
	api "github.com/nodal/controlplane/internal/gen/api"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/observability"
)

// Internal commerce (gola.md PART XVII).
//
// The shape of these responses carries one rule: a product's earning origin is
// SHOWN and never ACCEPTED. A seller is entitled to know which provenance
// category their revenue will land in before they list -- it decides whether a
// future payout policy can ever reach it -- but no request body anywhere here
// has a field for it.

func toAPIProduct(p commerce.Product) api.InternalProduct {
	origin, _ := commerce.EarningOrigin(p.Kind)
	out := api.InternalProduct{
		ProductId:       uuid.MustParse(p.ID.String()),
		SellerAccountId: uuid.MustParse(p.SellerAccountID.String()),
		Kind:            api.InternalProductKind(p.Kind),
		Title:           p.Title,
		Description:     ptr(p.Description),
		Price:           qtyString(p.Price),
		PlatformFeeBps:  ptr(int(p.PlatformFeeBPS)),
		Version:         p.Version,
		Status:          api.InternalProductStatus(p.Status),
		EarningOrigin:   api.CreditOrigin(origin),
		TermsFrozen:     ptr(p.TermsFrozen()),
		CreatedAt:       ptr(p.CreatedAt),
	}
	// The split is shown alongside the price so a seller never has to compute
	// the platform's share themselves and get the rounding direction wrong.
	if fee, proceeds, err := p.Split(); err == nil {
		out.PlatformFee = ptr(qtyString(fee))
		out.SellerProceeds = ptr(qtyString(proceeds))
	}
	if p.PublishedAt != nil {
		out.PublishedAt = ptr(p.PublishedAt.UTC())
	}
	return out
}

func toAPISeller(s commerce.Seller) api.InternalSeller {
	out := api.InternalSeller{
		AccountId:   uuid.MustParse(s.AccountID.String()),
		DisplayName: s.DisplayName,
		Status:      api.InternalSellerStatus(s.Status),
		CreatedAt:   ptr(s.CreatedAt),
	}
	if s.PayoutAccountID != nil {
		id := uuid.MustParse(s.PayoutAccountID.String())
		out.PayoutAccountId = &id
	}
	if s.SuspendedReason != "" {
		out.SuspendedReason = ptr(s.SuspendedReason)
	}
	return out
}

func toAPIInternalOrder(o commerce.Order) api.InternalOrder {
	out := api.InternalOrder{
		OrderId:         uuid.MustParse(o.ID.String()),
		ProductId:       uuid.MustParse(o.ProductID.String()),
		ProductVersion:  o.ProductVersion,
		BuyerAccountId:  uuid.MustParse(o.BuyerAccountID.String()),
		SellerAccountId: uuid.MustParse(o.SellerAccountID.String()),
		Price:           qtyString(o.Price),
		PlatformFee:     qtyString(o.PlatformFee),
		SellerProceeds:  qtyString(o.SellerProceeds),
		EarningOrigin:   api.CreditOrigin(o.EarningOrigin),
		CreatedAt:       ptr(o.CreatedAt),
	}
	if !o.EarningAccountID.IsZero() {
		id := uuid.MustParse(o.EarningAccountID.String())
		out.EarningAccountId = &id
	}
	if !o.JournalTxID.IsZero() {
		id := uuid.MustParse(o.JournalTxID.String())
		out.JournalTransactionId = &id
	}
	return out
}

// PostInternalSellers registers the calling account as a seller.
func (s *Server) PostInternalSellers(ctx context.Context, request api.PostInternalSellersRequestObject) (api.PostInternalSellersResponseObject, error) {
	if s.opts.Ports.Commerce == nil {
		return nil, errNotWired("internal commerce")
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	accountID, err := accountScopeWrite(ctx, request.Body.AccountId)
	if err != nil {
		return nil, err
	}
	cmd := RegisterSeller{
		AccountID:     accountID,
		DisplayName:   request.Body.DisplayName,
		CorrelationID: observability.CorrelationID(ctx),
	}
	if request.Body.PayoutAccountId != nil {
		// Attributing earnings to a DIFFERENT account is exactly how the
		// self-dealing check could be walked around, so the caller must own
		// that account too. The domain refuses the sale as well; this refuses
		// the setup.
		payoutID, perr := accountScopeWrite(ctx, *request.Body.PayoutAccountId)
		if perr != nil {
			return nil, perr
		}
		cmd.PayoutAccountID = &payoutID
	}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.InternalSeller, commandMeta, error) {
			seller, cerr := s.opts.Ports.Commerce.RegisterSeller(ctx, cmd)
			if cerr != nil {
				return api.InternalSeller{}, commandMeta{}, cerr
			}
			return toAPISeller(seller), commandMeta{
				Status:       http.StatusOK,
				ResourceType: "internal_seller",
				ResourceID:   seller.AccountID.String(),
			}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostInternalSellers200JSONResponse(res.Value), nil
}

// PostInternalProducts creates a product in DRAFT.
func (s *Server) PostInternalProducts(ctx context.Context, request api.PostInternalProductsRequestObject) (api.PostInternalProductsResponseObject, error) {
	if s.opts.Ports.Commerce == nil {
		return nil, errNotWired("internal commerce")
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	accountID, err := accountScopeWrite(ctx, request.Body.AccountId)
	if err != nil {
		return nil, err
	}
	price, err := money.ParseQuantity(request.Body.Price)
	if err != nil {
		return nil, validationError("price", "price must be an integer string of Credit base units")
	}
	kind := commerce.Kind(request.Body.Kind)
	if !kind.Valid() {
		return nil, validationError("kind", "unknown product kind")
	}
	cmd := CreateInternalProduct{
		SellerAccountID: accountID,
		Kind:            kind,
		Title:           request.Body.Title,
		Price:           price,
		IdempotencyKey:  request.Params.IdempotencyKey,
		CorrelationID:   observability.CorrelationID(ctx),
	}
	if request.Body.Description != nil {
		cmd.Description = *request.Body.Description
	}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.InternalProduct, commandMeta, error) {
			p, cerr := s.opts.Ports.Commerce.CreateProduct(ctx, cmd)
			if cerr != nil {
				return api.InternalProduct{}, commandMeta{}, cerr
			}
			return toAPIProduct(p), commandMeta{
				Status:       http.StatusCreated,
				ResourceType: "internal_product",
				ResourceID:   p.ID.String(),
			}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostInternalProducts201JSONResponse(res.Value), nil
}

// GetInternalProducts lists products currently for sale.
func (s *Server) GetInternalProducts(ctx context.Context, request api.GetInternalProductsRequestObject) (api.GetInternalProductsResponseObject, error) {
	if s.opts.Ports.Commerce == nil {
		return nil, errNotWired("internal commerce")
	}
	var kind commerce.Kind
	if request.Params.Kind != nil {
		kind = commerce.Kind(*request.Params.Kind)
		if !kind.Valid() {
			return nil, validationError("kind", "unknown product kind")
		}
	}
	limit := 50
	if request.Params.Limit != nil {
		limit = *request.Params.Limit
	}
	list, err := s.opts.Ports.Commerce.ListProducts(ctx, kind, limit)
	if err != nil {
		return nil, err
	}
	items := make([]api.InternalProduct, 0, len(list))
	for _, p := range list {
		items = append(items, toAPIProduct(p))
	}
	return api.GetInternalProducts200JSONResponse(api.InternalProductPage{Items: items}), nil
}

// GetInternalProductsProductId returns one product.
func (s *Server) GetInternalProductsProductId(ctx context.Context, request api.GetInternalProductsProductIdRequestObject) (api.GetInternalProductsProductIdResponseObject, error) {
	if s.opts.Ports.Commerce == nil {
		return nil, errNotWired("internal commerce")
	}
	id, err := commerce.ParseProductID(request.ProductId.String())
	if err != nil {
		return nil, validationError("productId", "productId must be a canonical UUID")
	}
	p, err := s.opts.Ports.Commerce.Product(ctx, id)
	if err != nil {
		return nil, err
	}
	// A DRAFT or WITHDRAWN product is the seller's own business, and this
	// response carries its price, its fee split and its seller. `ListActive`
	// shows only what is buyable; this read had no filter, so any customer
	// could read an unpublished catalogue entry by asking for its id.
	// NOT_FOUND rather than FORBIDDEN: a distinguishable refusal is a
	// membership oracle (F-41).
	if !p.Status.Sellable() && securityRequireAccountOwner(ctx, p.SellerAccountID.String()) != nil {
		return nil, errs.New(errs.CodeNotFound, "no such product").
			WithField("product_id", id.String())
	}
	return api.GetInternalProductsProductId200JSONResponse(toAPIProduct(p)), nil
}

// PostInternalProductsProductIdStatus publishes, pauses, resumes or withdraws.
func (s *Server) PostInternalProductsProductIdStatus(ctx context.Context, request api.PostInternalProductsProductIdStatusRequestObject) (api.PostInternalProductsProductIdStatusResponseObject, error) {
	if s.opts.Ports.Commerce == nil {
		return nil, errNotWired("internal commerce")
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	id, err := commerce.ParseProductID(request.ProductId.String())
	if err != nil {
		return nil, validationError("productId", "productId must be a canonical UUID")
	}
	to := commerce.Status(request.Body.Status)
	if !to.Valid() {
		return nil, validationError("status", "unknown product status")
	}
	// Tenant scoping: only the seller may move their own product. The port
	// reads it first for exactly this check, because "who owns this product"
	// is not in the request.
	current, err := s.opts.Ports.Commerce.Product(ctx, id)
	if err != nil {
		return nil, err
	}
	// Ownership only. Publishing, pausing or withdrawing a product is a write,
	// and an operator who needs to take a listing down does it through the
	// admin plane, where COMMERCE_PRODUCT_WITHDRAW gives it a reason and a
	// permanent record (F-102).
	if serr := securityRequireAccountOwner(ctx, current.SellerAccountID.String()); serr != nil {
		return nil, serr
	}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.InternalProduct, commandMeta, error) {
			p, cerr := s.opts.Ports.Commerce.SetProductStatus(ctx, id, to)
			if cerr != nil {
				return api.InternalProduct{}, commandMeta{}, cerr
			}
			return toAPIProduct(p), commandMeta{
				Status:       http.StatusOK,
				ResourceType: "internal_product",
				ResourceID:   p.ID.String(),
			}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostInternalProductsProductIdStatus200JSONResponse(res.Value), nil
}

// PostInternalProductsProductIdOrders buys a product with Credits.
func (s *Server) PostInternalProductsProductIdOrders(ctx context.Context, request api.PostInternalProductsProductIdOrdersRequestObject) (api.PostInternalProductsProductIdOrdersResponseObject, error) {
	if s.opts.Ports.Commerce == nil {
		return nil, errNotWired("internal commerce")
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	id, err := commerce.ParseProductID(request.ProductId.String())
	if err != nil {
		return nil, validationError("productId", "productId must be a canonical UUID")
	}
	accountID, err := accountScopeWrite(ctx, request.Body.AccountId)
	if err != nil {
		return nil, err
	}
	expected, err := money.ParseQuantity(request.Body.ExpectedPrice)
	if err != nil {
		return nil, validationError("expected_price",
			"expected_price must be an integer string of Credit base units")
	}
	cmd := PurchaseInternalProduct{
		ProductID:      id,
		BuyerAccountID: accountID,
		ExpectedPrice:  expected,
		IdempotencyKey: request.Params.IdempotencyKey,
		CorrelationID:  observability.CorrelationID(ctx),
	}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.InternalOrder, commandMeta, error) {
			o, cerr := s.opts.Ports.Commerce.Purchase(ctx, cmd)
			if cerr != nil {
				return api.InternalOrder{}, commandMeta{}, cerr
			}
			return toAPIInternalOrder(o), commandMeta{
				Status:       http.StatusCreated,
				ResourceType: "internal_commerce_order",
				ResourceID:   o.ID.String(),
			}, nil
		})
	if err != nil {
		return nil, err
	}
	if res.Replayed {
		return api.PostInternalProductsProductIdOrders200JSONResponse(res.Value), nil
	}
	return api.PostInternalProductsProductIdOrders201JSONResponse(res.Value), nil
}

// GetInternalOrders lists what an account bought or sold.
func (s *Server) GetInternalOrders(ctx context.Context, request api.GetInternalOrdersRequestObject) (api.GetInternalOrdersResponseObject, error) {
	if s.opts.Ports.Commerce == nil {
		return nil, errNotWired("internal commerce")
	}
	accountID, err := accountScope(ctx, request.Params.AccountId)
	if err != nil {
		return nil, err
	}
	asSeller := request.Params.Role != nil && *request.Params.Role == api.SELLER
	limit := 50
	if request.Params.Limit != nil {
		limit = *request.Params.Limit
	}
	list, err := s.opts.Ports.Commerce.Orders(ctx, accountID, asSeller, limit)
	if err != nil {
		return nil, err
	}
	items := make([]api.InternalOrder, 0, len(list))
	for _, o := range list {
		items = append(items, toAPIInternalOrder(o))
	}
	return api.GetInternalOrders200JSONResponse(api.InternalOrderPage{Items: items}), nil
}
