package commerce

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/credit"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// Poster is the part of internal/ledger this package uses.
type Poster interface {
	Post(ctx context.Context, tx pgx.Tx, p ledger.Posting) (ledger.PostResult, error)
}

// Credits is the part of internal/credit this package uses.
type Credits interface {
	Consume(ctx context.Context, tx pgx.Tx, r credit.ConsumeRequest) ([]credit.Allocation, error)
	RecordLot(ctx context.Context, tx pgx.Tx, r credit.RecordLotRequest) (credit.Lot, error)
	AssetID(ctx context.Context, q db.Querier) (assets.AssetID, error)
}

// CapabilityResolver reports which capability gates are currently ACTIVE.
//
// It is an interface rather than a value because activation is deployment
// state that changes without a restart, and a snapshot taken at construction
// would keep selling after the gate was pulled.
type CapabilityResolver interface {
	// ActiveCapabilities reads through the querier the caller supplies, which
	// inside Purchase is the purchase's own transaction. Reading through the
	// pool instead, while holding a transaction from that pool, is how a dozen
	// concurrent purchases deadlocked every connection (F-27).
	ActiveCapabilities(ctx context.Context, q db.Querier) (map[valuedomain.CapabilityKey]bool, error)
}

// CapMarketplace is the gate that must be ACTIVE for a purchase to commit.
//
// It is checked HERE, in the domain service, and not only in the Settlement
// Compiler in front of it. A gate that lives only at the HTTP edge is a gate a
// worker, a script or a future caller walks around without noticing, and this
// is the one operation in the package that moves value.
const CapMarketplace valuedomain.CapabilityKey = "MARKETPLACE"

// Service registers sellers, publishes products and executes purchases.
type Service struct {
	poster  Poster
	credits Credits
	auditor Audit
	clk     clock.Clock
	caps    CapabilityResolver
}

// Audit is the part of internal/audit this package uses. It appends inside
// the caller's transaction, so a purchase's audit row commits with the
// purchase or not at all.
type Audit interface {
	Append(ctx context.Context, tx pgx.Tx, e audit.Event) (audit.Appended, error)
}

// NewService returns a Service. No argument may be nil.
//
// The returned service has NO capability resolver, which means nothing is
// active and no purchase will commit. That is the correct default: a
// deployment that has not decided whether it runs a user-to-user marketplace
// has not decided yes.
func NewService(poster Poster, credits Credits, auditor Audit, clk clock.Clock) *Service {
	if poster == nil || credits == nil || auditor == nil || clk == nil {
		panic("commerce: NewService requires a poster, a credit service, an audit writer and a clock")
	}
	return &Service{poster: poster, credits: credits, auditor: auditor, clk: clk}
}

// SetCapabilityResolver attaches the deployment's gate state. Passing nil
// restores the fail-closed default.
func (s *Service) SetCapabilityResolver(r CapabilityResolver) { s.caps = r }

// requireMarketplace refuses unless the MARKETPLACE gate is ACTIVE.
func (s *Service) requireMarketplace(ctx context.Context, q db.Querier) error {
	var active map[valuedomain.CapabilityKey]bool
	if s.caps != nil {
		var err error
		if active, err = s.caps.ActiveCapabilities(ctx, q); err != nil {
			return err
		}
	}
	if active[CapMarketplace] {
		return nil
	}
	return errs.New(errs.CodeCapabilityNotApproved,
		"the internal marketplace is not enabled in this deployment; "+
			"users cannot yet buy from each other here").
		WithField("capability", string(CapMarketplace))
}

// RegisterSeller records an account's agreement to sell.
func (s *Service) RegisterSeller(ctx context.Context, tx pgx.Tx, sel Seller) (Seller, error) {
	if sel.AccountID.IsZero() {
		return Seller{}, errs.New(errs.CodeValidationFailed, "a seller needs an account")
	}
	if strings.TrimSpace(sel.DisplayName) == "" {
		return Seller{}, errs.New(errs.CodeValidationFailed, "a seller needs a display name")
	}
	if sel.PayoutAccountID != nil && *sel.PayoutAccountID == sel.AccountID {
		// Not an error worth failing over, but worth not storing: a payout
		// account equal to the selling account is the default, and recording
		// it as an override would make the override look meaningful.
		sel.PayoutAccountID = nil
	}
	var payout any
	if sel.PayoutAccountID != nil {
		payout = *sel.PayoutAccountID
	}
	err := tx.QueryRow(ctx,
		`INSERT INTO internal_sellers (account_id, display_name, status, payout_account_id)
		 VALUES ($1,$2,'ACTIVE',$3)
		 ON CONFLICT (account_id) DO UPDATE SET display_name = EXCLUDED.display_name
		 RETURNING status, created_at, updated_at`,
		sel.AccountID, sel.DisplayName, payout).Scan(&sel.Status, &sel.CreatedAt, &sel.UpdatedAt)
	if err != nil {
		return Seller{}, mapError(err)
	}
	return sel, nil
}

// Seller returns one seller.
func (s *Service) Seller(ctx context.Context, q db.Querier, accountID accounts.AccountID) (Seller, error) {
	var (
		sel    Seller
		status string
		payout *accounts.AccountID
		reason *string
	)
	err := q.QueryRow(ctx,
		`SELECT account_id, display_name, status, payout_account_id, suspended_reason, created_at, updated_at
		   FROM internal_sellers WHERE account_id = $1`, accountID).
		Scan(&sel.AccountID, &sel.DisplayName, &status, &payout, &reason, &sel.CreatedAt, &sel.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Seller{}, errs.New(errs.CodeNotFound, "this account is not registered as a seller").
				WithField("account_id", accountID.String())
		}
		return Seller{}, mapError(err)
	}
	sel.Status, sel.PayoutAccountID = SellerStatus(status), payout
	if reason != nil {
		sel.SuspendedReason = *reason
	}
	return sel, nil
}

// SetSellerStatus moves a seller between ACTIVE, SUSPENDED and CLOSED.
//
// It is the operator's control, driven by the dual-audited
// COMMERCE_SELLER_SUSPEND administrative action rather than by a customer
// request. Suspension stops NEW orders and touches nothing already earned:
// clawing back a completed sale is a ledger correction, which is a different
// action with a different approval, and conflating the two would let a
// moderation decision move money.
func (s *Service) SetSellerStatus(
	ctx context.Context, tx pgx.Tx, accountID accounts.AccountID, to SellerStatus, reason string,
) (Seller, error) {
	if !to.Valid() {
		return Seller{}, errs.Newf(errs.CodeValidationFailed, "unknown seller status %q", to)
	}
	if to != SellerActive && strings.TrimSpace(reason) == "" {
		return Seller{}, errs.New(errs.CodeValidationFailed,
			"suspending or closing a seller requires a reason; it is the only record of why")
	}
	sel, err := s.Seller(ctx, tx, accountID)
	if err != nil {
		return Seller{}, err
	}
	if sel.Status == to {
		return sel, nil
	}
	// CLOSED is terminal. A seller who can come back is SUSPENDED, and the two
	// mean different things to somebody reading an account's history.
	if sel.Status == SellerClosed {
		return Seller{}, errs.New(errs.CodeInvalidStateTransition,
			"this seller is CLOSED; that is terminal").
			WithField("account_id", accountID.String())
	}
	var stored *string
	if strings.TrimSpace(reason) != "" {
		r := reason
		stored = &r
	}
	updated := sel
	err = tx.QueryRow(ctx,
		`UPDATE internal_sellers SET status = $2, suspended_reason = $3
		  WHERE account_id = $1
		 RETURNING status, updated_at`,
		accountID, string(to), stored).Scan(&updated.Status, &updated.UpdatedAt)
	if err != nil {
		return Seller{}, mapError(err)
	}
	updated.SuspendedReason = reason
	return updated, nil
}

// CreateProduct records a product in DRAFT.
func (s *Service) CreateProduct(ctx context.Context, tx pgx.Tx, p Product) (Product, error) {
	p.Status = StatusDraft
	if p.Version == 0 {
		p.Version = 1
	}
	if err := p.Validate(); err != nil {
		return Product{}, err
	}
	sel, err := s.Seller(ctx, tx, p.SellerAccountID)
	if err != nil {
		return Product{}, err
	}
	if !sel.Status.CanSell() {
		return Product{}, errs.Newf(errs.CodeForbidden,
			"this seller is %s and cannot list products", sel.Status).
			WithField("account_id", sel.AccountID.String())
	}
	if p.ID.IsZero() {
		p.ID = NewProductID()
	}
	meta, err := json.Marshal(orEmpty(p.Metadata))
	if err != nil {
		return Product{}, errs.Wrap(err, errs.CodeValidationFailed, "product metadata is not JSON-encodable")
	}
	err = tx.QueryRow(ctx,
		`INSERT INTO internal_products
		   (id, seller_account_id, kind, title, description, price, version, platform_fee_bps, status, metadata)
		 VALUES ($1,$2,$3,$4,$5,$6::numeric,$7,$8,'DRAFT',$9)
		 RETURNING created_at, updated_at`,
		p.ID, p.SellerAccountID, string(p.Kind), p.Title, p.Description,
		p.Price.String(), p.Version, int(p.PlatformFeeBPS), meta).Scan(&p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return Product{}, mapError(err)
	}
	return p, nil
}

// Publish makes a product buyable and freezes its terms.
func (s *Service) Publish(ctx context.Context, tx pgx.Tx, productID ProductID) (Product, error) {
	return s.setStatus(ctx, tx, productID, StatusActive)
}

// SetStatus moves a product through its lifecycle.
func (s *Service) SetStatus(ctx context.Context, tx pgx.Tx, productID ProductID, to Status) (Product, error) {
	return s.setStatus(ctx, tx, productID, to)
}

func (s *Service) setStatus(ctx context.Context, tx pgx.Tx, productID ProductID, to Status) (Product, error) {
	if !to.Valid() {
		return Product{}, errs.Newf(errs.CodeValidationFailed, "unknown product status %q", to)
	}
	p, err := s.productForUpdate(ctx, tx, productID)
	if err != nil {
		return Product{}, err
	}
	if p.Status == to {
		return p, nil
	}
	if !CanTransition(p.Status, to) {
		return Product{}, errs.Newf(errs.CodeInvalidStateTransition,
			"a product cannot go %s -> %s", p.Status, to).WithField("product_id", productID.String())
	}
	set := `status = $2`
	if to == StatusActive && p.PublishedAt == nil {
		// published_at is set once and never moves; it is the instant the
		// terms froze.
		set += `, published_at = now()`
	}
	updated, err := scanProduct(tx.QueryRow(ctx,
		`UPDATE internal_products SET `+set+` WHERE id = $1 RETURNING `+productColumns,
		productID, string(to)))
	if err != nil {
		return Product{}, mapError(err)
	}
	return updated, nil
}

// Purchase executes a sale.
//
// Everything happens in the caller's transaction: the buyer's Credits are
// consumed, the seller's earning is recorded with the provenance the product
// kind dictates, and the order is written. If any part fails, none of it
// happened.
func (s *Service) Purchase(ctx context.Context, tx pgx.Tx, r PurchaseRequest) (Order, error) {
	if err := r.Validate(); err != nil {
		return Order{}, err
	}
	if tx == nil {
		return Order{}, errs.New(errs.CodeInternal, "commerce: Purchase requires a transaction")
	}
	// The gate is checked before the idempotency lookup so that a replay of a
	// purchase made while the marketplace was enabled does not keep working
	// after it was switched off. A replay is not a different act.
	if err := s.requireMarketplace(ctx, tx); err != nil {
		return Order{}, err
	}
	if existing, found, err := s.orderByIdempotencyKey(ctx, tx, r.IdempotencyKey); err != nil {
		return Order{}, err
	} else if found {
		// Whose replay this is has to be asked, for the reason recorded in
		// internal/payout: the key is globally unique and the boundary's own
		// idempotency record is per actor, so another account's order is what
		// comes back otherwise -- buyer, seller, price, platform fee and
		// proceeds (F-106).
		if existing.BuyerAccountID != r.BuyerAccountID {
			return Order{}, errs.New(errs.CodeInvalidIdempotencyReuse,
				"commerce: idempotency key belongs to another account").
				WithField("idempotency_key", r.IdempotencyKey)
		}
		return existing, nil
	}

	p, err := s.productForUpdate(ctx, tx, r.ProductID)
	if err != nil {
		return Order{}, err
	}
	if !p.Status.Sellable() {
		return Order{}, errs.Newf(errs.CodeAssetRestricted,
			"this product is %s and cannot be bought", p.Status).
			WithField("product_id", p.ID.String())
	}
	// The buyer agreed to a price. A price that has changed since is a refusal,
	// not a surprise charge.
	if p.Price.Cmp(r.ExpectedPrice) != 0 {
		return Order{}, errs.Newf(errs.CodeConflict,
			"the price changed: this product now costs %s, and you agreed to %s", p.Price, r.ExpectedPrice).
			WithField("product_id", p.ID.String()).
			WithField("current_price", p.Price.String()).
			WithField("expected_price", r.ExpectedPrice.String())
	}

	sel, err := s.Seller(ctx, tx, p.SellerAccountID)
	if err != nil {
		return Order{}, err
	}
	if !sel.Status.CanSell() {
		return Order{}, errs.Newf(errs.CodeForbidden,
			"this seller is %s and is not taking orders", sel.Status)
	}
	earner := sel.EarningAccount()

	// Self-dealing is the attack this package exists to prevent: buying from
	// yourself would turn non-withdrawable Credits into withdrawable
	// creator-earning provenance at will. The database refuses it too; this is
	// the check that produces a comprehensible error.
	if r.BuyerAccountID == p.SellerAccountID || r.BuyerAccountID == earner {
		return Order{}, errs.New(errs.CodeForbidden,
			"an account cannot buy from itself; a sale to yourself would manufacture withdrawable provenance out of Credits that are not").
			WithField("product_id", p.ID.String())
	}

	origin, ok := EarningOrigin(p.Kind)
	if !ok {
		return Order{}, errs.Newf(errs.CodeInternal,
			"product kind %s has no declared earning provenance", p.Kind)
	}
	fee, proceeds, err := p.Split()
	if err != nil {
		return Order{}, err
	}
	creditAsset, err := s.credits.AssetID(ctx, tx)
	if err != nil {
		return Order{}, err
	}

	// One posting, one asset, one domain: Credits move from the buyer to the
	// earner and the platform. No conversion is declared because none happens
	// -- this is a transfer inside INTERNAL_CREDIT, and the ledger's own
	// balance and negative-balance triggers are the whole check.
	entries := []ledger.Entry{
		{
			Account: ledger.CustomerAccount(r.BuyerAccountID, ledger.CodeCreditBalance, creditAsset),
			Side:    ledger.Credit, Quantity: p.Price,
		},
	}
	if proceeds.IsPositive() {
		entries = append(entries, ledger.Entry{
			Account: ledger.CustomerAccount(earner, ledger.CodeCreditBalance, creditAsset),
			Side:    ledger.Debit, Quantity: proceeds,
		})
	}
	if fee.IsPositive() {
		entries = append(entries, ledger.Entry{
			Account: ledger.PlatformAccount(ledger.CodePlatformFeeReceivable, creditAsset),
			Side:    ledger.Debit, Quantity: fee,
		})
	}

	post, err := s.poster.Post(ctx, tx, ledger.Posting{
		Kind:           ledger.KindInternalPurchase,
		IdempotencyKey: "commerce:" + r.IdempotencyKey,
		Reference:      ledger.FinancialEventReference{Type: "internal_commerce_order", ID: r.IdempotencyKey},
		EffectiveAt:    r.EffectiveAt,
		CorrelationID:  r.CorrelationID,
		Description:    "internal commerce purchase",
		Entries:        entries,
		Metadata: map[string]any{
			"product_id":     p.ID.String(),
			"product_kind":   string(p.Kind),
			"earning_origin": string(origin),
		},
	})
	if err != nil {
		return Order{}, err
	}

	// The buyer's lots. Ordinary spending rules apply: value whose funding is
	// disputed or reversed cannot buy anything.
	if _, err := s.credits.Consume(ctx, tx, credit.ConsumeRequest{
		AccountID:                r.BuyerAccountID,
		Quantity:                 p.Price,
		JournalTxID:              post.TransactionID,
		Reference:                credit.Reference{Type: "internal_commerce_order", ID: r.IdempotencyKey},
		Reason:                   "internal commerce purchase",
		RequireSpendableFinality: true,
	}); err != nil {
		return Order{}, err
	}

	// The seller's earning, with the provenance the product kind dictates.
	//
	// Finality is REVERSIBLE, not SETTLED: the Credits that paid for this may
	// themselves be backed by a card payment still inside its dispute window,
	// and an earning cannot be more final than the money behind it. It is
	// spendable, which is what the product needs, and not payout-eligible,
	// which is the conservative half.
	if proceeds.IsPositive() {
		if _, err := s.credits.RecordLot(ctx, tx, credit.RecordLotRequest{
			AccountID:        earner,
			Quantity:         proceeds,
			Origin:           origin,
			Finality:         valuedomain.FinalityReversible,
			Reference:        credit.Reference{Type: "internal_commerce_order", ID: r.IdempotencyKey},
			FundingReference: &credit.Reference{Type: "internal_product", ID: p.ID.String()},
			JournalTxID:      post.TransactionID,
			Reason:           "sale of " + string(p.Kind),
		}); err != nil {
			return Order{}, err
		}
	}

	o := Order{
		ID: NewOrderID(), ProductID: p.ID, ProductVersion: p.Version,
		BuyerAccountID: r.BuyerAccountID, SellerAccountID: p.SellerAccountID,
		EarningAccountID: earner,
		Price:            p.Price, PlatformFee: fee, SellerProceeds: proceeds,
		EarningOrigin: origin, JournalTxID: post.TransactionID,
		IdempotencyKey: r.IdempotencyKey,
	}
	err = tx.QueryRow(ctx,
		`INSERT INTO internal_commerce_orders
		   (id, product_id, product_version, buyer_account_id, seller_account_id, earning_account_id,
		    price, platform_fee, seller_proceeds, earning_origin, journal_transaction_id, idempotency_key)
		 VALUES ($1,$2,$3,$4,$5,$6,$7::numeric,$8::numeric,$9::numeric,$10,$11,$12)
		 RETURNING created_at`,
		o.ID, o.ProductID, o.ProductVersion, o.BuyerAccountID, o.SellerAccountID, o.EarningAccountID,
		o.Price.String(), o.PlatformFee.String(), o.SellerProceeds.String(),
		string(o.EarningOrigin), o.JournalTxID, o.IdempotencyKey).Scan(&o.CreatedAt)
	if err != nil {
		return Order{}, mapError(err)
	}
	o.CreatedAt = o.CreatedAt.UTC()

	// Proof (STAGE 15; PARTS 87-88). The order goes into BOTH parties' audit
	// streams, in this transaction with the money: a purchase is one event to
	// the buyer and a different one to the earner, and each is entitled to
	// prove their own half without being handed the other's history.
	if err := s.recordOrder(ctx, tx, o, p); err != nil {
		return Order{}, err
	}
	return o, nil
}

// recordOrder appends the order to the buyer's and the earner's audit streams.
func (s *Service) recordOrder(ctx context.Context, tx pgx.Tx, o Order, p Product) error {
	actorType, actorID := actorFrom(ctx)
	payload, err := json.Marshal(map[string]any{
		"order_id":               o.ID.String(),
		"product_id":             o.ProductID.String(),
		"product_version":        o.ProductVersion,
		"product_kind":           string(p.Kind),
		"buyer_account_id":       o.BuyerAccountID.String(),
		"seller_account_id":      o.SellerAccountID.String(),
		"earning_account_id":     o.EarningAccountID.String(),
		"price":                  o.Price.String(),
		"platform_fee":           o.PlatformFee.String(),
		"seller_proceeds":        o.SellerProceeds.String(),
		"earning_origin":         string(o.EarningOrigin),
		"journal_transaction_id": o.JournalTxID.String(),
	})
	if err != nil {
		return errs.Wrap(err, errs.CodeInternal, "commerce: encode order audit payload")
	}
	for _, ev := range []audit.Event{
		{
			Stream: audit.AccountStream(o.BuyerAccountID.String()),
			Action: "internal_commerce.purchase", Reason: "internal marketplace purchase",
		},
		{
			Stream: audit.AccountStream(o.EarningAccountID.String()),
			Action: "internal_commerce.sale", Reason: "internal marketplace sale",
		},
	} {
		ev.ActorType, ev.ActorID = actorType, actorID
		ev.ResourceType, ev.ResourceID = "internal_commerce_order", o.ID.String()
		ev.Payload, ev.OccurredAt = payload, o.CreatedAt
		if _, err := s.auditor.Append(ctx, tx, ev); err != nil {
			return err
		}
	}
	return nil
}

func orEmpty(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

func mapError(err error) error {
	if err == nil {
		return nil
	}
	switch db.SQLState(err) {
	case "IC001":
		return errs.Wrap(err, errs.CodeInternal,
			"an order's accounting does not match the posting it names")
	case "IC002":
		return errs.Wrap(err, errs.CodeForbidden,
			"this product was published; its price, fee and kind are fixed for this version")
	}
	if db.IsUniqueViolation(err) {
		return errs.Wrap(err, errs.CodeConflict, "commerce: duplicate record")
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return errs.New(errs.CodeNotFound, "commerce: not found")
	}
	if mapped := ledger.MapError(err); mapped != nil {
		return mapped
	}
	return errs.Wrap(err, errs.CodeInternal, "commerce: database error")
}

// actorFrom reports the principal on the context, or the system actor when
// there is none. A purchase always has an actor; a seeding script does not.
func actorFrom(ctx context.Context) (string, string) {
	if p, ok := security.PrincipalFrom(ctx); ok && p.SubjectID != "" {
		return string(p.ActorType), p.SubjectID
	}
	return "SYSTEM", "commerce-service"
}
