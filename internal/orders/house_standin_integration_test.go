//go:build integration

package orders

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/econ"
)

// Since migration 00759 a HOUSE order exists only as the result of a treasury
// action: at COMMIT every house order must be named by a PLACE_ORDER action of
// its house book (TY007). The treasury writes that action in the transaction
// that places the order (treasury.Service.PlaceOrder). This package cannot
// import the treasury (the treasury imports it), so its own tests of the order
// book's house rules -- reservations, crossing, idempotency -- stand in for the
// treasury: they place the house order through the service and record the
// action the treasury would, in the same transaction. The treasury's controls
// themselves are the treasury package's tests' business.

// standInHouseService is the house service the stand-in signing policy names.
const standInHouseService = "orders-itest-house"

// standInTreasury registers a house book with the treasury tables (a
// PRINCIPAL book valued in the sandbox dollar with a version 1 signing policy)
// unless it already is, in its own transaction. It returns its error rather
// than failing the test, so it may run on any goroutine.
func (f *fixture) standInTreasury(house econ.HouseAccountID) error {
	return f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		usd, err := econ.EnsureSandboxDollar(ctx, tx)
		if err != nil {
			return err
		}
		var registered econ.HouseAccountID
		err = tx.QueryRow(ctx, `INSERT INTO treasury_books (house_account_id, class, valuation_asset_id, registered_by_actor_type,
			registered_by_actor_id, registered_at) VALUES ($1, 'PRINCIPAL', $2, 'OPERATOR', 'orders-itest', $3)
			ON CONFLICT (house_account_id) DO NOTHING RETURNING house_account_id`, house, usd.ID, f.clk.Now()).Scan(&registered)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		doc, err := json.Marshal(map[string]any{
			"kind": "treasury.signing_policy.v1", "house_account_id": house.String(), "key_custody": "NONE_SANDBOX",
			"service_actor_id": standInHouseService, "operators_allowed": false,
		})
		if err != nil {
			return err
		}
		sum := sha256.Sum256(doc)
		_, err = tx.Exec(ctx, `INSERT INTO treasury_signing_policies (id, house_account_id, version, key_custody, service_actor_id,
			operators_allowed, policy, policy_hash, actor_type, actor_id, reason, effective_at)
			VALUES ($1, $2, 1, 'NONE_SANDBOX', $3, false, $4, $5, 'OPERATOR', 'orders-itest', 'orders test stand-in', $6)`,
			uuid.New(), house, standInHouseService, doc, hex.EncodeToString(sum[:]), f.clk.Now())
		return err
	})
}

// recordStandInAction records the PLACE_ORDER action the treasury would have
// recorded for an order it placed: APPLIED (with the risk decision an applied
// house order names) when the book took the order, REJECTED with the book's
// reason when it refused it. A replayed order already has its action.
func (f *fixture) recordStandInAction(ctx context.Context, tx pgx.Tx, house econ.HouseAccountID, out CommandOutcome) error {
	if out.Order.ID.IsZero() {
		return nil
	}
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM treasury_actions WHERE order_id = $1 AND kind = 'PLACE_ORDER')`,
		out.Order.ID).Scan(&exists); err != nil || exists {
		return err
	}
	usd := econ.SandboxDollarID()
	reqHash := sha256.Sum256([]byte("stand-in:" + out.Order.ID.String()))
	actionID := uuid.New()
	if out.Rejection != nil {
		reason := ReasonOf(out.Rejection)
		if reason == "" {
			reason = "TREASURY_ORDER_REFUSED"
		}
		_, err := tx.Exec(ctx, `INSERT INTO treasury_actions (id, house_account_id, kind, activity, outcome, rejection_code, reason_codes,
			actor_type, actor_id, signing_policy_version, idempotency_key, request_hash, order_id, occurred_at)
			VALUES ($1, $2, 'PLACE_ORDER', 'MARKET_MAKING', 'REJECTED', 'TREASURY_ORDER_REFUSED', $3, 'SERVICE', $4, 1, $5, $6, $7, $8)`,
			actionID, house, []string{reason}, standInHouseService, "stand-in:"+out.Order.ID.String(), reqHash[:], out.Order.ID, f.clk.Now())
		return err
	}
	decisionID := uuid.New()
	if _, err := tx.Exec(ctx, `INSERT INTO treasury_risk_decisions (id, house_account_id, intent_kind, instrument_id, asset_id,
		risk_increasing, stage, verdict, reason_codes, treasury_codes, input, decision, limits, input_hash, decision_hash,
		risk_policy_version, risk_policy_hash, evaluator_version, treasury_policy_version, treasury_policy_hash, evaluated_at)
		VALUES ($1, $2, 'PROVIDE_LIQUIDITY', $3, $4, false, 'PRE_TRADE', 'ALLOW', '{}', '{}', '{}', '{}', '{}', 'stand-in', 'stand-in',
		        'orders-itest', 'stand-in', 'orders-itest', 1, $5, $6)`,
		decisionID, house, out.Order.InstrumentID, usd, hex.EncodeToString(reqHash[:]), f.clk.Now()); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `INSERT INTO treasury_actions (id, house_account_id, kind, activity, outcome, reason_codes, actor_type,
		actor_id, signing_policy_version, idempotency_key, request_hash, risk_decision_id, order_id, occurred_at)
		VALUES ($1, $2, 'PLACE_ORDER', 'MARKET_MAKING', 'APPLIED', '{}', 'SERVICE', $3, 1, $4, $5, $6, $7, $8)`,
		actionID, house, standInHouseService, "stand-in:"+out.Order.ID.String(), reqHash[:], decisionID, out.Order.ID, f.clk.Now())
	return err
}

// submitHouse places a house order as the treasury would, in one transaction:
// the order book's own SubmitHouseOrder, then the stand-in action. Like
// Pipeline.SubmitHouse it returns a recorded refusal as its error after the
// transaction committed.
func (f *fixture) submitHouse(r HouseOrderRequest) (CommandOutcome, error) {
	if err := f.standInTreasury(r.HouseAccountID); err != nil {
		return CommandOutcome{}, err
	}
	var out CommandOutcome
	if err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		o, err := f.svc.SubmitHouseOrder(ctx, tx, r)
		if err != nil {
			return err
		}
		out = o
		return f.recordStandInAction(ctx, tx, r.HouseAccountID, o)
	}); err != nil {
		return CommandOutcome{}, err
	}
	if out.Rejection != nil {
		return out, out.Rejection
	}
	return out, nil
}
