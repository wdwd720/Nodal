package httpapi

import (
	"context"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/commerce"
	"github.com/nodal/controlplane/internal/money"
)

// CommercePort is the creator economy (gola.md PART XVII).
//
// It follows the same rule as every other port here: one method per domain
// call, no transaction handling above this line, and a nil port answers
// UNSUPPORTED rather than pretending.
//
// Note what is NOT on this interface: nothing accepts a provenance. A caller
// cannot ask for their earning to be recorded as one origin rather than
// another, because the product kind decides it and the kind is fixed at
// publication. An API that let a client name the origin would hand an attacker
// the exact thing the package exists to prevent.
type CommercePort interface {
	RegisterSeller(ctx context.Context, r RegisterSeller) (commerce.Seller, error)
	CreateProduct(ctx context.Context, r CreateInternalProduct) (commerce.Product, error)
	SetProductStatus(ctx context.Context, productID commerce.ProductID, to commerce.Status) (commerce.Product, error)
	Product(ctx context.Context, productID commerce.ProductID) (commerce.Product, error)
	ListProducts(ctx context.Context, kind commerce.Kind, limit int) ([]commerce.Product, error)
	Purchase(ctx context.Context, r PurchaseInternalProduct) (commerce.Order, error)
	Orders(ctx context.Context, accountID accounts.AccountID, asSeller bool, limit int) ([]commerce.Order, error)
}

// RegisterSeller is the command behind POST /internal-sellers.
type RegisterSeller struct {
	AccountID       accounts.AccountID
	DisplayName     string
	PayoutAccountID *accounts.AccountID
	CorrelationID   string
}

// CreateInternalProduct is the command behind POST /internal-products.
type CreateInternalProduct struct {
	SellerAccountID accounts.AccountID
	Kind            commerce.Kind
	Title           string
	Description     string
	Price           money.Quantity
	PlatformFeeBPS  money.BPS
	IdempotencyKey  string
	CorrelationID   string
}

// PurchaseInternalProduct is the command behind
// POST /internal-products/{productId}/orders.
type PurchaseInternalProduct struct {
	ProductID      commerce.ProductID
	BuyerAccountID accounts.AccountID
	ExpectedPrice  money.Quantity
	IdempotencyKey string
	CorrelationID  string
}
