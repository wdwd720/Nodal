package commerce

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/valuedomain"
)

const productColumns = `id, seller_account_id, kind, title, description, price::text, version,
	platform_fee_bps, status, metadata, published_at, created_at, updated_at`

func scanProduct(row pgx.Row) (Product, error) {
	var (
		p            Product
		kind, status string
		price        string
		feeBPS       int
		meta         []byte
	)
	if err := row.Scan(&p.ID, &p.SellerAccountID, &kind, &p.Title, &p.Description, &price,
		&p.Version, &feeBPS, &status, &meta, &p.PublishedAt, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return Product{}, err
	}
	p.Kind, p.Status = Kind(kind), Status(status)
	p.PlatformFeeBPS = money.BPS(feeBPS)
	v, err := money.ParseQuantity(price)
	if err != nil {
		return Product{}, errs.Wrap(err, errs.CodeInternal, "commerce: price is not an integer")
	}
	p.Price = v
	if len(meta) > 0 {
		_ = json.Unmarshal(meta, &p.Metadata)
	}
	p.CreatedAt, p.UpdatedAt = p.CreatedAt.UTC(), p.UpdatedAt.UTC()
	return p, nil
}

// Product returns one product.
func (s *Service) Product(ctx context.Context, q db.Querier, id ProductID) (Product, error) {
	p, err := scanProduct(q.QueryRow(ctx, `SELECT `+productColumns+` FROM internal_products WHERE id = $1`, id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Product{}, errs.New(errs.CodeNotFound, "product not found").
				WithField("product_id", id.String())
		}
		return Product{}, mapError(err)
	}
	return p, nil
}

func (s *Service) productForUpdate(ctx context.Context, tx pgx.Tx, id ProductID) (Product, error) {
	p, err := scanProduct(tx.QueryRow(ctx,
		`SELECT `+productColumns+` FROM internal_products WHERE id = $1 FOR UPDATE`, id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Product{}, errs.New(errs.CodeNotFound, "product not found").
				WithField("product_id", id.String())
		}
		return Product{}, mapError(err)
	}
	return p, nil
}

// ListActive returns buyable products, newest first.
func (s *Service) ListActive(ctx context.Context, q db.Querier, kind Kind, limit int) ([]Product, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var (
		rows pgx.Rows
		err  error
	)
	if kind == "" {
		rows, err = q.Query(ctx, listActiveQuery, limit)
	} else {
		rows, err = q.Query(ctx, listActiveByKindQuery, string(kind), limit)
	}
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	var out []Product
	for rows.Next() {
		p, serr := scanProduct(rows)
		if serr != nil {
			return nil, mapError(serr)
		}
		out = append(out, p)
	}
	return out, mapError(rows.Err())
}

const listActiveQuery = `SELECT ` + productColumns + `
	  FROM internal_products WHERE status = 'ACTIVE'
	 ORDER BY created_at DESC LIMIT $1`

const listActiveByKindQuery = `SELECT ` + productColumns + `
	  FROM internal_products WHERE status = 'ACTIVE' AND kind = $1
	 ORDER BY created_at DESC LIMIT $2`

const orderColumns = `id, product_id, product_version, buyer_account_id, seller_account_id,
	earning_account_id, price::text, platform_fee::text, seller_proceeds::text, earning_origin,
	journal_transaction_id, idempotency_key, created_at`

func scanOrder(row pgx.Row) (Order, error) {
	var (
		o                    Order
		price, fee, proceeds string
		origin               string
	)
	if err := row.Scan(&o.ID, &o.ProductID, &o.ProductVersion, &o.BuyerAccountID, &o.SellerAccountID,
		&o.EarningAccountID, &price, &fee, &proceeds, &origin,
		&o.JournalTxID, &o.IdempotencyKey, &o.CreatedAt); err != nil {
		return Order{}, err
	}
	o.EarningOrigin = valuedomain.CreditOrigin(origin)
	for dst, src := range map[*money.Quantity]string{
		&o.Price: price, &o.PlatformFee: fee, &o.SellerProceeds: proceeds,
	} {
		v, err := money.ParseQuantity(src)
		if err != nil {
			return Order{}, errs.Wrap(err, errs.CodeInternal, "commerce: order amount is not an integer")
		}
		*dst = v
	}
	o.CreatedAt = o.CreatedAt.UTC()
	return o, nil
}

// Order returns one order.
func (s *Service) Order(ctx context.Context, q db.Querier, id OrderID) (Order, error) {
	o, err := scanOrder(q.QueryRow(ctx,
		`SELECT `+orderColumns+` FROM internal_commerce_orders WHERE id = $1`, id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Order{}, errs.New(errs.CodeNotFound, "order not found").WithField("order_id", id.String())
		}
		return Order{}, mapError(err)
	}
	return o, nil
}

func (s *Service) orderByIdempotencyKey(ctx context.Context, q db.Querier, key string) (Order, bool, error) {
	o, err := scanOrder(q.QueryRow(ctx,
		`SELECT `+orderColumns+` FROM internal_commerce_orders WHERE idempotency_key = $1`, key))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Order{}, false, nil
		}
		return Order{}, false, mapError(err)
	}
	return o, true, nil
}

// OrdersByBuyer returns what an account has bought, newest first.
func (s *Service) OrdersByBuyer(ctx context.Context, q db.Querier, buyer accounts.AccountID, limit int) ([]Order, error) {
	return s.orders(ctx, q, ordersByBuyerQuery, buyer, limit)
}

// OrdersBySeller returns what an account has sold, newest first.
func (s *Service) OrdersBySeller(ctx context.Context, q db.Querier, seller accounts.AccountID, limit int) ([]Order, error) {
	return s.orders(ctx, q, ordersBySellerQuery, seller, limit)
}

func (s *Service) orders(ctx context.Context, q db.Querier, query string, account accounts.AccountID, limit int) ([]Order, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := q.Query(ctx, query, account, limit)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	var out []Order
	for rows.Next() {
		o, serr := scanOrder(rows)
		if serr != nil {
			return nil, mapError(serr)
		}
		out = append(out, o)
	}
	return out, mapError(rows.Err())
}

const ordersByBuyerQuery = `SELECT ` + orderColumns + `
	  FROM internal_commerce_orders WHERE buyer_account_id = $1
	 ORDER BY created_at DESC LIMIT $2`

const ordersBySellerQuery = `SELECT ` + orderColumns + `
	  FROM internal_commerce_orders WHERE seller_account_id = $1
	 ORDER BY created_at DESC LIMIT $2`

// VerifyEarnings checks the invariant that ties this package to provenance:
// what a seller has been paid through commerce equals what their earning lots
// of commerce origins were issued.
//
// A divergence means an earning was recorded without an order or an order paid
// without recording provenance. Either is a reconciliation incident.
func (s *Service) VerifyEarnings(ctx context.Context, q db.Querier, earner accounts.AccountID) error {
	var orderSum, lotSum string
	err := q.QueryRow(ctx,
		`SELECT
		   coalesce((SELECT sum(seller_proceeds) FROM internal_commerce_orders
		              WHERE earning_account_id = $1), 0)::text,
		   coalesce((SELECT sum(l.quantity) FROM credit_lots l
		              WHERE l.account_id = $1
		                AND l.origin IN ('CREATOR_EARNING','DATA_SALE_EARNING','AGENT_SERVICE_EARNING')
		                AND l.funding_reference_type = 'internal_product'), 0)::text`,
		earner).Scan(&orderSum, &lotSum)
	if err != nil {
		return mapError(err)
	}
	if orderSum != lotSum {
		return errs.Newf(errs.CodeReconciliationRequired,
			"commerce earnings do not reconcile: orders paid %s, provenance lots hold %s",
			orderSum, lotSum).
			WithField("account_id", earner.String()).
			WithField("order_sum", orderSum).
			WithField("lot_sum", lotSum)
	}
	return nil
}
