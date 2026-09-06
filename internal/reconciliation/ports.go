package reconciliation

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/admin"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/chain"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/execution"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/positions"
)

// The ports below are the narrow slices of other packages this engine needs.
// They are declared here (consumer side) so the engine can be exercised
// against fakes; the production types satisfy them without adaptation.

// Orders is the slice of *execution.Repository the engine uses.
type Orders interface {
	Get(ctx context.Context, q db.Querier, orderID execution.OrderID) (execution.Order, error)
	GetByPlan(ctx context.Context, q db.Querier, planID string) (execution.Order, error)
	Transition(ctx context.Context, tx pgx.Tx, orderID execution.OrderID, to execution.OrderStatus, ev execution.TransitionEvidence) (execution.Order, error)
	RecordFill(ctx context.Context, tx pgx.Tx, f execution.Fill) (execution.Fill, error)
	ListFills(ctx context.Context, q db.Querier, orderID execution.OrderID) ([]execution.Fill, error)
	MarkFillPosted(ctx context.Context, tx pgx.Tx, fillID execution.FillID, journalTxID string) error
	MarkPositionApplied(ctx context.Context, tx pgx.Tx, fillID execution.FillID, at time.Time) error
}

// Attempts is the slice of *execution.AttemptRepository the engine uses.
type Attempts interface {
	Get(ctx context.Context, q db.Querier, attemptID execution.AttemptID) (execution.Attempt, error)
	ListForOrder(ctx context.Context, q db.Querier, orderID execution.OrderID) ([]execution.Attempt, error)
	ListRecoverable(ctx context.Context, q db.Querier, limit int) ([]execution.Attempt, error)
	Update(ctx context.Context, tx pgx.Tx, attemptID execution.AttemptID, p execution.AttemptPatch) (execution.Attempt, error)
	FindBySignature(ctx context.Context, q db.Querier, txSignature string) (execution.Attempt, error)
}

// Ledger is the slice of *ledger.Service the engine uses. There is
// deliberately no balance-writing method: the only way to change a balance is
// to post a transaction.
type Ledger interface {
	Post(ctx context.Context, tx pgx.Tx, p ledger.Posting) (ledger.PostResult, error)
	Balance(ctx context.Context, q db.Querier, ref ledger.AccountRef) (money.Quantity, error)
	BalancesForOwner(ctx context.Context, q db.Querier, ownerType ledger.OwnerType, ownerID string) ([]ledger.AccountBalance, error)
}

// Positions is the slice of positions.LotEngine the engine uses. Positions are
// only ever changed by acquiring or disposing lots — never overwritten
// (PART 195).
type Positions interface {
	Acquire(ctx context.Context, tx pgx.Tx, in positions.AcquireLot) (positions.Lot, error)
	Dispose(ctx context.Context, tx pgx.Tx, d positions.Disposal) ([]positions.LotDisposition, positions.RealizedPnL, error)
	VerifyAgainstLedger(ctx context.Context, q db.Querier, accountID accounts.AccountID) ([]positions.Drift, error)
}

// Reservations is the slice of *capital.Service the engine uses. The recovery
// path never reserves: it only consumes what an order already holds or
// releases what it turned out not to need.
type Reservations interface {
	Get(ctx context.Context, q db.Querier, rid capital.ReservationID) (capital.Reservation, error)
	ConsumeFinal(ctx context.Context, tx pgx.Tx, rid capital.ReservationID, qty money.Quantity, usdMinor int64, orderID string) (capital.Reservation, error)
	Release(ctx context.Context, tx pgx.Tx, rid capital.ReservationID, reason string) (capital.Reservation, error)
}

// Approvals verifies an admin_actions approval before a material manual
// resolution is accepted. *admin.Service satisfies it.
type Approvals interface {
	VerifyApproved(ctx context.Context, q db.Querier, approvalID string, kind admin.Kind, targetID string) (admin.Approval, error)
}

// AssetRegistry reads asset metadata (decimals) for evidence rendering.
type AssetRegistry interface {
	Get(ctx context.Context, q db.Querier, assetID assets.AssetID) (assets.Asset, error)
}

// USDValuer converts an exact asset quantity into USD minor units for the
// materiality test. It is optional: without one the engine falls back to the
// per-asset dust thresholds and treats anything above dust as material.
type USDValuer interface {
	ValueUSD(ctx context.Context, q db.Querier, assetID assets.AssetID, qty money.Quantity, at time.Time) (money.USD, error)
}

// Observers is the pair of chain observers plus the agreement policy that
// decides what they may jointly assert (PART 196). Secondary may be nil, in
// which case every resolution is the degraded single-observer path and
// absence can never be proven.
type Observers struct {
	Primary   chain.ChainObserver
	Secondary chain.ChainObserver
	Policy    chain.AgreementPolicy
}

// Adapters maps a venue name to its execution adapter, used to ask the venue
// what it thinks happened to a submission (PART 48 "query provider").
type Adapters map[string]execution.ExecutionAdapter

// Wallets resolves an account's wallet addresses. Reconciliation needs the
// address to read chain balances and wallet activity.
type Wallets interface {
	ListForAccount(ctx context.Context, q db.Querier, accountID accounts.AccountID) ([]Wallet, error)
	Get(ctx context.Context, q db.Querier, walletID string) (Wallet, error)
}

// Wallet is the minimal wallet projection reconciliation needs.
type Wallet struct {
	ID        string // uuid text
	AccountID accounts.AccountID
	Chain     string
	Address   string
	Status    string
}

// SQLWallets reads wallets straight from the table. It exists so the engine
// needs no dependency on internal/wallet, which owns provider delegation and
// is written by another team.
type SQLWallets struct{}

// NewSQLWallets returns the default wallet reader.
func NewSQLWallets() *SQLWallets { return &SQLWallets{} }

var _ Wallets = (*SQLWallets)(nil)

const walletColumns = `id::text, account_id, chain, address, status`

// ListForAccount returns an account's wallets.
func (SQLWallets) ListForAccount(ctx context.Context, q db.Querier, accountID accounts.AccountID) ([]Wallet, error) {
	rows, err := q.Query(ctx, `SELECT `+walletColumns+` FROM wallets WHERE account_id = $1 ORDER BY created_at, id`, accountID)
	if err != nil {
		return nil, dbErr("list wallets", err)
	}
	defer rows.Close()
	out := []Wallet{}
	for rows.Next() {
		var w Wallet
		if err := rows.Scan(&w.ID, &w.AccountID, &w.Chain, &w.Address, &w.Status); err != nil {
			return nil, dbErr("scan wallet", err)
		}
		out = append(out, w)
	}
	if err := rows.Err(); err != nil {
		return nil, dbErr("list wallets", err)
	}
	return out, nil
}

// Get returns one wallet.
func (SQLWallets) Get(ctx context.Context, q db.Querier, walletID string) (Wallet, error) {
	var w Wallet
	if err := q.QueryRow(ctx, `SELECT `+walletColumns+` FROM wallets WHERE id = $1::uuid`, walletID).
		Scan(&w.ID, &w.AccountID, &w.Chain, &w.Address, &w.Status); err != nil {
		return Wallet{}, dbErr("get wallet", err)
	}
	return w, nil
}
