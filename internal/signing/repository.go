package signing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/wallet"
)

type decisionKind struct{}

// DecisionID identifies a signing_decisions row.
type DecisionID = id.ID[decisionKind]

// NewDecisionID returns a fresh decision id.
func NewDecisionID() DecisionID { return id.New[decisionKind]() }

// ParseDecisionID parses the canonical form.
func ParseDecisionID(s string) (DecisionID, error) { return id.Parse[decisionKind](s) }

// Row types mirror only the columns the boundary needs. Nullable uuid
// columns are read as text ("" when NULL) so no pointer scanning is needed.

type attemptRow struct {
	ID                   string
	OrderID              string
	PlanID               string
	AttemptNo            int
	WalletID             string
	QuoteID              string
	UnsignedTxHash       []byte
	RecentBlockhash      string
	LastValidBlockHeight *int64
	SimulationRef        string
	SimulationOK         *bool
	Status               string
	CorrelationID        string
}

type planRow struct {
	ID              string
	IntentID        string
	Status          string
	HardConstraints []byte
	RiskDecisionID  string
	QuoteID         string
	PlanHash        []byte
	DryRun          bool
	ApprovedAt      *time.Time
}

type intentRow struct {
	ID             string
	AccountID      string
	Status         string
	ReservationID  string
	RiskDecisionID string
	PlanID         string
}

type quoteRow struct {
	ID            string
	IntentID      string
	InputAssetID  assets.AssetID
	InputQuantity money.Quantity
	OutputAssetID assets.AssetID
	MinimumOutput money.Quantity
	SlippageBPS   int64
	ExpiresAt     time.Time
}

type reservationRow struct {
	ID               string
	AccountID        string
	AssetID          assets.AssetID
	IntentID         string
	LockedByOrderID  string
	Quantity         money.Quantity
	ConsumedQuantity money.Quantity
	Status           string
	ExpiresAt        time.Time
}

type riskRow struct {
	ID                   string
	IntentID             string
	Decision             string
	Stage                string
	ResultingConstraints []byte
}

type decisionRow struct {
	ID               DecisionID
	AttemptID        string
	PlanID           string
	IntentID         string
	RiskDecisionID   string
	WalletID         string
	ExpectedTxHash   []byte
	InspectedTxHash  []byte
	Decision         string
	ReasonCodes      []string
	Checks           []byte
	InspectorVersion string
	RequestedBy      string
	DecidedAt        time.Time
}

type resultRow struct {
	ID              string
	DecisionID      DecisionID
	AttemptID       string
	WalletID        string
	Provider        string
	ProviderSignRef string
	IdempotencyKey  string
	SignedTx        []byte
	SignedTxHash    []byte
	Signature       []byte
	RetryClass      string
	SignedAt        time.Time
}

// repository is the SQL access of the boundary. It holds no connection.
type repository struct {
	wallets *wallet.Repository
	assets  *assets.Repository
}

func newRepository() *repository {
	return &repository{wallets: wallet.NewRepository(), assets: assets.NewRepository()}
}

func notFound(what, idv string) error {
	return errs.Newf(errs.CodeNotFound, "%s not found", what).WithField("id", idv)
}

// lockAttempt loads the attempt under FOR UPDATE (serializes Sign per attempt).
func (r *repository) lockAttempt(ctx context.Context, tx pgx.Tx, attemptID string) (attemptRow, error) {
	return r.scanAttempt(tx.QueryRow(ctx, attemptSelect+` WHERE id = $1 FOR UPDATE`, attemptID), attemptID)
}

// getAttempt loads the attempt without a lock.
func (r *repository) getAttempt(ctx context.Context, q db.Querier, attemptID string) (attemptRow, error) {
	return r.scanAttempt(q.QueryRow(ctx, attemptSelect+` WHERE id = $1`, attemptID), attemptID)
}

const attemptSelect = `SELECT id::text, order_id::text, plan_id::text, attempt_no, wallet_id::text, quote_id::text,
	unsigned_tx_hash, coalesce(recent_blockhash,''), last_valid_block_height, coalesce(simulation_ref,''), simulation_ok,
	status, correlation_id FROM execution_attempts`

func (r *repository) scanAttempt(row pgx.Row, attemptID string) (attemptRow, error) {
	var a attemptRow
	err := row.Scan(&a.ID, &a.OrderID, &a.PlanID, &a.AttemptNo, &a.WalletID, &a.QuoteID, &a.UnsignedTxHash,
		&a.RecentBlockhash, &a.LastValidBlockHeight, &a.SimulationRef, &a.SimulationOK, &a.Status, &a.CorrelationID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return attemptRow{}, notFound("execution attempt", attemptID)
		}
		return attemptRow{}, fmt.Errorf("signing: load attempt: %w", err)
	}
	return a, nil
}

func (r *repository) getPlan(ctx context.Context, q db.Querier, planID string) (planRow, error) {
	var p planRow
	err := q.QueryRow(ctx, `SELECT id::text, intent_id::text, status, hard_constraints, coalesce(risk_decision_id::text,''),
		coalesce(quote_id::text,''), plan_hash, dry_run, approved_at FROM execution_plans WHERE id = $1`, planID).
		Scan(&p.ID, &p.IntentID, &p.Status, &p.HardConstraints, &p.RiskDecisionID, &p.QuoteID, &p.PlanHash, &p.DryRun, &p.ApprovedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return planRow{}, notFound("execution plan", planID)
		}
		return planRow{}, fmt.Errorf("signing: load plan: %w", err)
	}
	return p, nil
}

func (r *repository) getIntent(ctx context.Context, q db.Querier, intentID string) (intentRow, error) {
	var i intentRow
	err := q.QueryRow(ctx, `SELECT id::text, account_id::text, status, coalesce(reservation_id::text,''),
		coalesce(risk_decision_id::text,''), coalesce(plan_id::text,'') FROM trade_intents WHERE id = $1`, intentID).
		Scan(&i.ID, &i.AccountID, &i.Status, &i.ReservationID, &i.RiskDecisionID, &i.PlanID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return intentRow{}, notFound("trade intent", intentID)
		}
		return intentRow{}, fmt.Errorf("signing: load intent: %w", err)
	}
	return i, nil
}

func (r *repository) getQuote(ctx context.Context, q db.Querier, quoteID string) (quoteRow, error) {
	var qr quoteRow
	err := q.QueryRow(ctx, `SELECT id::text, coalesce(intent_id::text,''), input_asset_id, input_quantity, output_asset_id,
		minimum_output, slippage_bps, expires_at FROM quotes WHERE id = $1`, quoteID).
		Scan(&qr.ID, &qr.IntentID, &qr.InputAssetID, &qr.InputQuantity, &qr.OutputAssetID, &qr.MinimumOutput, &qr.SlippageBPS, &qr.ExpiresAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return quoteRow{}, notFound("quote", quoteID)
		}
		return quoteRow{}, fmt.Errorf("signing: load quote: %w", err)
	}
	qr.ExpiresAt = qr.ExpiresAt.UTC()
	return qr, nil
}

func (r *repository) getReservation(ctx context.Context, q db.Querier, reservationID string) (reservationRow, error) {
	var rr reservationRow
	err := q.QueryRow(ctx, `SELECT id::text, account_id::text, asset_id, coalesce(intent_id::text,''), coalesce(locked_by_order_id::text,''),
		quantity, consumed_quantity, status, expires_at FROM asset_reservations WHERE id = $1`, reservationID).
		Scan(&rr.ID, &rr.AccountID, &rr.AssetID, &rr.IntentID, &rr.LockedByOrderID, &rr.Quantity, &rr.ConsumedQuantity, &rr.Status, &rr.ExpiresAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return reservationRow{}, notFound("asset reservation", reservationID)
		}
		return reservationRow{}, fmt.Errorf("signing: load reservation: %w", err)
	}
	rr.ExpiresAt = rr.ExpiresAt.UTC()
	return rr, nil
}

func (r *repository) getRiskDecision(ctx context.Context, q db.Querier, riskID string) (riskRow, error) {
	var rk riskRow
	err := q.QueryRow(ctx, `SELECT id::text, coalesce(intent_id::text,''), decision, stage, resulting_constraints
		FROM risk_decisions WHERE id = $1`, riskID).
		Scan(&rk.ID, &rk.IntentID, &rk.Decision, &rk.Stage, &rk.ResultingConstraints)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return riskRow{}, notFound("risk decision", riskID)
		}
		return riskRow{}, fmt.Errorf("signing: load risk decision: %w", err)
	}
	return rk, nil
}

// findDecision returns the latest decision for an attempt, or nil.
func (r *repository) findDecision(ctx context.Context, q db.Querier, attemptID string) (*decisionRow, error) {
	row := q.QueryRow(ctx, decisionSelect+` WHERE attempt_id = $1 ORDER BY created_at DESC, id DESC LIMIT 1`, attemptID)
	d, err := scanDecision(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("signing: find decision: %w", err)
	}
	return &d, nil
}

func (r *repository) getDecision(ctx context.Context, q db.Querier, decisionID DecisionID) (decisionRow, error) {
	d, err := scanDecision(q.QueryRow(ctx, decisionSelect+` WHERE id = $1`, decisionID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return decisionRow{}, notFound("signing decision", decisionID.String())
		}
		return decisionRow{}, fmt.Errorf("signing: get decision: %w", err)
	}
	return d, nil
}

const decisionSelect = `SELECT id, attempt_id::text, plan_id::text, intent_id::text, risk_decision_id::text, wallet_id::text,
	expected_tx_hash, inspected_tx_hash, decision, reason_codes, checks, inspector_version, requested_by_service, decided_at
	FROM signing_decisions`

func scanDecision(row pgx.Row) (decisionRow, error) {
	var d decisionRow
	if err := row.Scan(&d.ID, &d.AttemptID, &d.PlanID, &d.IntentID, &d.RiskDecisionID, &d.WalletID, &d.ExpectedTxHash,
		&d.InspectedTxHash, &d.Decision, &d.ReasonCodes, &d.Checks, &d.InspectorVersion, &d.RequestedBy, &d.DecidedAt); err != nil {
		return decisionRow{}, err
	}
	d.DecidedAt = d.DecidedAt.UTC()
	return d, nil
}

func (r *repository) insertDecision(ctx context.Context, tx pgx.Tx, d decisionRow) error {
	codes := d.ReasonCodes
	if codes == nil {
		codes = []string{}
	}
	checks := d.Checks
	if len(checks) == 0 {
		checks = []byte(`[]`)
	}
	_, err := tx.Exec(ctx, `INSERT INTO signing_decisions (id, attempt_id, plan_id, intent_id, risk_decision_id, wallet_id,
			expected_tx_hash, inspected_tx_hash, decision, reason_codes, checks, inspector_version, requested_by_service, decided_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`,
		d.ID, d.AttemptID, d.PlanID, d.IntentID, d.RiskDecisionID, d.WalletID, d.ExpectedTxHash, d.InspectedTxHash,
		d.Decision, codes, checks, d.InspectorVersion, d.RequestedBy, d.DecidedAt.UTC())
	if err != nil {
		if db.IsForeignKeyViolation(err) {
			return errs.Wrap(err, errs.CodeNotFound, "signing: a referenced row does not exist")
		}
		return fmt.Errorf("signing: insert decision: %w", err)
	}
	return nil
}

// findResult returns the signing result for a decision, or nil.
func (r *repository) findResult(ctx context.Context, q db.Querier, decisionID DecisionID) (*resultRow, error) {
	var res resultRow
	err := q.QueryRow(ctx, `SELECT id::text, decision_id, attempt_id::text, wallet_id::text, provider, coalesce(provider_sign_ref,''),
		idempotency_key, signed_tx, signed_tx_hash, signature, retry_class, signed_at FROM signing_results WHERE decision_id = $1`, decisionID).
		Scan(&res.ID, &res.DecisionID, &res.AttemptID, &res.WalletID, &res.Provider, &res.ProviderSignRef, &res.IdempotencyKey,
			&res.SignedTx, &res.SignedTxHash, &res.Signature, &res.RetryClass, &res.SignedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("signing: find result: %w", err)
	}
	res.SignedAt = res.SignedAt.UTC()
	return &res, nil
}

func (r *repository) insertResult(ctx context.Context, tx pgx.Tx, res resultRow) error {
	_, err := tx.Exec(ctx, `INSERT INTO signing_results (id, decision_id, attempt_id, wallet_id, provider, provider_sign_ref,
			idempotency_key, signed_tx, signed_tx_hash, signature, retry_class, signed_at)
		VALUES ($1,$2,$3,$4,$5,NULLIF($6,''),$7,$8,$9,$10,$11,$12)`,
		id.New[id.Any](), res.DecisionID, res.AttemptID, res.WalletID, res.Provider, res.ProviderSignRef, res.IdempotencyKey,
		res.SignedTx, res.SignedTxHash, res.Signature, res.RetryClass, res.SignedAt.UTC())
	if err != nil {
		if db.IsUniqueViolation(err) {
			return errs.Wrap(err, errs.CodeConflict, "signing: a result already exists for this attempt")
		}
		return fmt.Errorf("signing: insert result: %w", err)
	}
	return nil
}

// insertSecurityEvent records a signing_rejection (HIGH) security event.
func (r *repository) insertSecurityEvent(ctx context.Context, tx pgx.Tx, accountID string, detail map[string]any, requestID string, at time.Time) error {
	payload, err := json.Marshal(detail)
	if err != nil {
		return fmt.Errorf("signing: encode security event: %w", err)
	}
	var acct any
	if accountID != "" {
		acct = accountID
	}
	if _, err := tx.Exec(ctx, `INSERT INTO security_events (id, kind, severity, account_id, detail, request_id, occurred_at)
		VALUES ($1, 'signing_rejection', 'HIGH', $2, $3, NULLIF($4,''), $5)`,
		id.New[id.Any](), acct, payload, requestID, at.UTC()); err != nil {
		return fmt.Errorf("signing: insert security event: %w", err)
	}
	return nil
}
