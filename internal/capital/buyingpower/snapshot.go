package buyingpower

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/money"
)

// Snapshot is the typed input of Evaluate: everything the engine reads,
// keyed by asset. Quantities are exact base units. Assets must carry
// metadata for every asset referenced by any map.
type Snapshot struct {
	AccountID     accounts.AccountID
	AccountStatus accounts.Status
	// QuoteAsset is the USD-pegged registry asset prices are quoted in.
	QuoteAsset assets.Asset
	Assets     map[assets.AssetID]assets.Asset

	Balances        map[assets.AssetID]money.Quantity // WALLET ledger balance
	Reserved        map[assets.AssetID]money.Quantity // asset_reservation_totals.reserved
	Holds           map[assets.AssetID]money.Quantity // Σ active withdrawal_holds
	Pending         map[assets.AssetID]money.Quantity // deposits not yet available, at expected quantity
	EligibleFunding map[assets.AssetID]money.Quantity // AVAILABLE deposits with withdrawal_eligible
	Withdrawals     map[assets.AssetID]money.Quantity // withdrawals not REJECTED/FAILED/CANCELLED

	KillSwitches         []KillSwitchState
	ReconciliationBlocks []ReconciliationBlock
}

// assetIDs returns the sorted union of every asset referenced.
func (s Snapshot) assetIDs() []assets.AssetID {
	seen := map[assets.AssetID]struct{}{}
	var out []assets.AssetID
	add := func(m map[assets.AssetID]money.Quantity) {
		for a := range m {
			if _, ok := seen[a]; !ok {
				seen[a] = struct{}{}
				out = append(out, a)
			}
		}
	}
	add(s.Balances)
	add(s.Reserved)
	add(s.Holds)
	add(s.Pending)
	add(s.EligibleFunding)
	add(s.Withdrawals)
	sort.Slice(out, func(i, j int) bool { return id.Compare(out[i], out[j]) < 0 })
	return out
}

// Pending deposit states: provider-confirmed money that is not yet AVAILABLE
// under policy (FINANCIAL_MODEL §4). Earlier states have no committed
// amount and terminal states are not pending.
const pendingDepositStates = `('PROVIDER_CONFIRMED','SETTLEMENT_OBSERVED','RECONCILED')`

// LoadSnapshot reads every input for accountID through q. For a
// point-in-time consistent view pass a REPEATABLE READ (or SERIALIZABLE)
// transaction; through a pool each query sees its own snapshot. The
// account must exist (NOT_FOUND otherwise); the quote asset must be a
// USD-pegged stablecoin (INTERNAL otherwise, as a wiring error).
func (e *Engine) LoadSnapshot(ctx context.Context, q db.Querier, accountID accounts.AccountID) (Snapshot, error) {
	if accountID.IsZero() {
		return Snapshot{}, errs.New(errs.CodeValidationFailed, "account_id required")
	}
	snap := Snapshot{
		AccountID:       accountID,
		Assets:          map[assets.AssetID]assets.Asset{},
		Balances:        map[assets.AssetID]money.Quantity{},
		Reserved:        map[assets.AssetID]money.Quantity{},
		Holds:           map[assets.AssetID]money.Quantity{},
		Pending:         map[assets.AssetID]money.Quantity{},
		EligibleFunding: map[assets.AssetID]money.Quantity{},
		Withdrawals:     map[assets.AssetID]money.Quantity{},
	}
	now := e.clk.Now()

	var status accounts.Status
	if err := q.QueryRow(ctx, `SELECT status FROM accounts WHERE id = $1`, accountID).Scan(&status); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Snapshot{}, errs.New(errs.CodeNotFound, "account not found").WithField("account_id", accountID.String())
		}
		return Snapshot{}, fmt.Errorf("buyingpower: account: %w", err)
	}
	snap.AccountStatus = status

	quote, err := e.assets.Get(ctx, q, e.quoteAssetID)
	if err != nil {
		return Snapshot{}, errs.Wrap(err, errs.CodeInternal, "buyingpower: quote asset not registered")
	}
	snap.QuoteAsset = quote

	queries := []struct {
		dst map[assets.AssetID]money.Quantity
		sql string
		arg []any
	}{
		{snap.Balances, `SELECT la.asset_id, lb.balance FROM ledger_accounts la JOIN ledger_balances lb ON lb.ledger_account_id = la.id
			WHERE la.owner_type = 'CUSTOMER' AND la.owner_id = $1 AND la.code = 'WALLET'`, []any{accountID}},
		{snap.Reserved, `SELECT asset_id, reserved FROM asset_reservation_totals WHERE account_id = $1 AND reserved > 0`, []any{accountID}},
		{snap.Holds, `SELECT asset_id, sum(quantity) FROM withdrawal_holds
			WHERE account_id = $1 AND released_at IS NULL AND (expires_at IS NULL OR expires_at > $2) GROUP BY asset_id`, []any{accountID, now}},
		{snap.Pending, `SELECT expected_asset_id, sum(coalesce(expected_quantity, observed_quantity, 0)) FROM deposits
			WHERE account_id = $1 AND status IN ` + pendingDepositStates + ` GROUP BY expected_asset_id`, []any{accountID}},
		{snap.EligibleFunding, `SELECT expected_asset_id, sum(coalesce(observed_quantity, expected_quantity, 0)) FROM deposits
			WHERE account_id = $1 AND status = 'AVAILABLE' AND withdrawal_eligible GROUP BY expected_asset_id`, []any{accountID}},
		{snap.Withdrawals, `SELECT asset_id, sum(quantity) FROM withdrawals
			WHERE account_id = $1 AND status NOT IN ('REJECTED','FAILED','CANCELLED') GROUP BY asset_id`, []any{accountID}},
	}
	for _, qq := range queries {
		if err := readQuantities(ctx, q, qq.sql, qq.arg, qq.dst); err != nil {
			return Snapshot{}, err
		}
	}

	for _, aid := range snap.assetIDs() {
		a, err := e.assets.Get(ctx, q, aid)
		if err != nil {
			return Snapshot{}, fmt.Errorf("buyingpower: asset %s: %w", aid, err)
		}
		snap.Assets[aid] = a
	}

	if snap.KillSwitches, err = e.killSwitches.Active(ctx, q, accountID); err != nil {
		return Snapshot{}, fmt.Errorf("buyingpower: kill switches: %w", err)
	}
	if snap.ReconciliationBlocks, err = e.reconciliation.Blocks(ctx, q, accountID); err != nil {
		return Snapshot{}, fmt.Errorf("buyingpower: reconciliation blocks: %w", err)
	}
	return snap, nil
}

// readQuantities runs a (asset_id, quantity) query into dst, dropping zero
// rows.
func readQuantities(ctx context.Context, q db.Querier, sql string, args []any, dst map[assets.AssetID]money.Quantity) error {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return fmt.Errorf("buyingpower: read inputs: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var a assets.AssetID
		var qty money.Quantity
		if err := rows.Scan(&a, &qty); err != nil {
			return fmt.Errorf("buyingpower: scan inputs: %w", err)
		}
		if qty.IsZero() {
			continue
		}
		dst[a] = qty
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("buyingpower: read inputs: %w", err)
	}
	return nil
}
