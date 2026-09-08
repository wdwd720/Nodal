package httpapi

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/commerce"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/settlement"
)

// The internal-commerce adapter.
//
// A purchase is one transaction and nothing else will do: the buyer's Credits
// are consumed, the seller's earning is recorded with the provenance the
// product kind dictates, and the order is written. This adapter is where that
// boundary lives, because the handler above it must not know that three
// things happened.

type commerceAdapter struct {
	svc  *commerce.Service
	db   *db.DB
	clk  clock.Clock
	deps NativeEconomyDeps
}

func (a commerceAdapter) RegisterSeller(ctx context.Context, r RegisterSeller) (commerce.Seller, error) {
	var out commerce.Seller
	err := a.db.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		var cerr error
		out, cerr = a.svc.RegisterSeller(ctx, tx, commerce.Seller{
			AccountID:       r.AccountID,
			DisplayName:     r.DisplayName,
			PayoutAccountID: r.PayoutAccountID,
		})
		return cerr
	})
	return out, err
}

func (a commerceAdapter) CreateProduct(ctx context.Context, r CreateInternalProduct) (commerce.Product, error) {
	var out commerce.Product
	err := a.db.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		var cerr error
		out, cerr = a.svc.CreateProduct(ctx, tx, commerce.Product{
			SellerAccountID: r.SellerAccountID,
			Kind:            r.Kind,
			Title:           r.Title,
			Description:     r.Description,
			Price:           r.Price,
			PlatformFeeBPS:  r.PlatformFeeBPS,
		})
		return cerr
	})
	return out, err
}

func (a commerceAdapter) SetProductStatus(ctx context.Context, productID commerce.ProductID, to commerce.Status) (commerce.Product, error) {
	var out commerce.Product
	err := a.db.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		var cerr error
		out, cerr = a.svc.SetStatus(ctx, tx, productID, to)
		return cerr
	})
	return out, err
}

func (a commerceAdapter) Product(ctx context.Context, productID commerce.ProductID) (commerce.Product, error) {
	return a.svc.Product(ctx, a.db, productID)
}

func (a commerceAdapter) ListProducts(ctx context.Context, kind commerce.Kind, limit int) ([]commerce.Product, error) {
	return a.svc.ListActive(ctx, a.db, kind, limit)
}

func (a commerceAdapter) Purchase(ctx context.Context, r PurchaseInternalProduct) (commerce.Order, error) {
	// Through the compiler first. internal/commerce checks the MARKETPLACE
	// gate itself and will refuse again if this is somehow bypassed; what the
	// compiler adds is the legal router's determination, the jurisdiction,
	// the verification level and the agent-authority question, none of which
	// the domain service can see.
	if _, cerr := a.deps.compileRoute(ctx, compileContext{
		Action: settlement.ActionPurchaseInternalService,
		Subject: settlement.Subject{
			Type: settlement.SubjectInternalProduct, ID: r.ProductID.String(),
		},
		AccountID:      r.BuyerAccountID,
		Amount:         r.ExpectedPrice.String(),
		IdempotencyKey: r.IdempotencyKey,
		CorrelationID:  r.CorrelationID,
	}); cerr != nil {
		return commerce.Order{}, cerr
	}

	var out commerce.Order
	err := a.db.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		var cerr error
		out, cerr = a.svc.Purchase(ctx, tx, commerce.PurchaseRequest{
			ProductID:      r.ProductID,
			BuyerAccountID: r.BuyerAccountID,
			ExpectedPrice:  r.ExpectedPrice,
			IdempotencyKey: r.IdempotencyKey,
			EffectiveAt:    a.clk.Now(),
			CorrelationID:  r.CorrelationID,
		})
		return cerr
	})
	return out, err
}

func (a commerceAdapter) Orders(ctx context.Context, accountID accounts.AccountID, asSeller bool, limit int) ([]commerce.Order, error) {
	if asSeller {
		return a.svc.OrdersBySeller(ctx, a.db, accountID, limit)
	}
	return a.svc.OrdersByBuyer(ctx, a.db, accountID, limit)
}
