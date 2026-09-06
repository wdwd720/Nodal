package funding

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/event"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
)

// OutboxEnqueuer writes envelopes to the transactional outbox inside the
// caller's transaction. *event.Outbox satisfies it.
type OutboxEnqueuer interface {
	Enqueue(ctx context.Context, tx pgx.Tx, topic string, events ...event.Envelope) error
}

// Repository persists deposits and their transitions. Every status change
// goes through Transition/TransitionWithPatch, which write the transition
// row, the outbox event and the audit event in the caller's transaction.
type Repository struct {
	clk    clock.Clock
	outbox OutboxEnqueuer
	audit  audit.Writer
}

// NewRepository builds a Repository. All dependencies are required.
func NewRepository(clk clock.Clock, outbox OutboxEnqueuer, aud audit.Writer) (*Repository, error) {
	if clk == nil || outbox == nil || aud == nil {
		return nil, errs.New(errs.CodeValidationFailed, "funding: clock, outbox and audit writer are required")
	}
	return &Repository{clk: clk, outbox: outbox, audit: aud}, nil
}

// Transitioner is the fixed transition contract (FINANCIAL_MODEL §7).
type Transitioner interface {
	Transition(ctx context.Context, tx pgx.Tx, id DepositID, to Status, ev TransitionEvidence) (Deposit, error)
}

var _ Transitioner = (*Repository)(nil)

const depositColumns = `id, account_id, funding_source_id::text, provider, provider_session_id, provider_ref, status,
	fiat_amount_minor, fiat_currency, expected_asset_id, expected_quantity::text, observed_quantity::text,
	destination_wallet_id::text, destination_address, tx_signature, chain_slot, fraud_state, reversible_until,
	buying_power_eligible, withdrawal_eligible, availability_policy_version, journal_transaction_id, reversal_journal_transaction_id,
	idempotency_key, correlation_id, created_at, session_created_at, customer_action_at, provider_processing_at, provider_confirmed_at,
	settlement_observed_at, reconciled_at, available_at, failed_at, expired_at, cancelled_at, reversed_at, review_required_at, updated_at`

func scanDeposit(row pgx.Row) (Deposit, error) {
	var (
		d                                                                                        Deposit
		fundingSource, sessionID, providerRef, fiatCurrency, expectedQ, observedQ, walletID, sig *string
		policyVersion, correlation                                                               *string
		status, fraud                                                                            string
	)
	err := row.Scan(
		&d.ID, &d.AccountID, &fundingSource, &d.Provider, &sessionID, &providerRef, &status,
		&d.FiatAmountMinor, &fiatCurrency, &d.ExpectedAssetID, &expectedQ, &observedQ,
		&walletID, &d.DestinationAddress, &sig, &d.ChainSlot, &fraud, &d.ReversibleUntil,
		&d.BuyingPowerEligible, &d.WithdrawalEligible, &policyVersion, &d.JournalTransactionID, &d.ReversalJournalTransactionID,
		&d.IdempotencyKey, &correlation, &d.CreatedAt, &d.SessionCreatedAt, &d.CustomerActionAt, &d.ProviderProcessingAt, &d.ProviderConfirmedAt,
		&d.SettlementObservedAt, &d.ReconciledAt, &d.AvailableAt, &d.FailedAt, &d.ExpiredAt, &d.CancelledAt, &d.ReversedAt, &d.ReviewRequiredAt, &d.UpdatedAt,
	)
	if err != nil {
		return Deposit{}, err
	}
	d.Status, d.FraudState = Status(status), FraudState(fraud)
	d.FundingSourceID, d.ProviderSessionID, d.ProviderRef = deref(fundingSource), deref(sessionID), deref(providerRef)
	d.FiatCurrency, d.DestinationWalletID, d.TxSignature = deref(fiatCurrency), deref(walletID), deref(sig)
	d.AvailabilityPolicyVersion, d.CorrelationID = deref(policyVersion), deref(correlation)
	if d.ExpectedQuantity, err = optionalQuantity(expectedQ); err != nil {
		return Deposit{}, err
	}
	if d.ObservedQuantity, err = optionalQuantity(observedQ); err != nil {
		return Deposit{}, err
	}
	return d, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func optionalQuantity(s *string) (*money.Quantity, error) {
	if s == nil {
		return nil, nil
	}
	q, err := money.ParseQuantity(*s)
	if err != nil {
		return nil, fmt.Errorf("funding: stored quantity %q: %w", *s, err)
	}
	return &q, nil
}

func dbErr(op string, err error) error {
	if db.IsRetryable(err) {
		return err // let db.InTx re-run the transaction
	}
	return errs.Wrap(err, errs.CodeInternal, "funding: "+op)
}

// Create inserts a CREATED deposit, idempotent on IdempotencyKey: a second
// call with the same key returns the stored deposit and created=false when
// it belongs to the same account, and INVALID_IDEMPOTENCY_REUSE otherwise.
// A fresh deposit is announced on the outbox and audited.
func (r *Repository) Create(ctx context.Context, tx pgx.Tx, in CreateDeposit, ev TransitionEvidence) (dep Deposit, created bool, err error) {
	if err := in.Validate(); err != nil {
		return Deposit{}, false, err
	}
	if err := ev.Validate(); err != nil {
		return Deposit{}, false, err
	}
	now := r.clk.Now().UTC()
	depositID := NewDepositID()
	tag, err := tx.Exec(ctx, `INSERT INTO deposits (id, account_id, provider, status, fiat_amount_minor, fiat_currency, expected_asset_id,
			destination_wallet_id, destination_address, idempotency_key, correlation_id, fraud_state, created_at, updated_at)
		VALUES ($1, $2, $3, 'CREATED', $4, NULLIF($5, ''), $6, NULLIF($7, '')::uuid, $8, $9, NULLIF($10, ''), 'NONE', $11, $11)
		ON CONFLICT (idempotency_key) DO NOTHING`,
		depositID, in.AccountID, in.Provider, in.FiatAmountMinor, strings.ToLower(in.FiatCurrency), in.ExpectedAssetID,
		in.DestinationWalletID, in.DestinationAddress, in.IdempotencyKey, in.CorrelationID, now)
	if err != nil {
		return Deposit{}, false, dbErr("insert deposit", err)
	}
	existing, found, err := r.GetByIdempotencyKey(ctx, tx, in.IdempotencyKey)
	if err != nil {
		return Deposit{}, false, err
	}
	if !found {
		return Deposit{}, false, errs.New(errs.CodeInternal, "funding: deposit vanished after insert")
	}
	if tag.RowsAffected() == 0 {
		if existing.AccountID != in.AccountID || existing.Provider != in.Provider {
			return Deposit{}, false, errs.New(errs.CodeInvalidIdempotencyReuse, "funding: idempotency key was used for a different deposit").
				WithField("idempotency_key", in.IdempotencyKey)
		}
		return existing, false, nil
	}
	if err := r.announce(ctx, tx, existing, "", StatusCreated, TransitionID{}, ev, now, "funding.deposit.created"); err != nil {
		return Deposit{}, false, err
	}
	return existing, true, nil
}

// Get returns a deposit by id.
func (r *Repository) Get(ctx context.Context, q db.Querier, depositID DepositID) (Deposit, error) {
	return r.get(ctx, q, depositID, false)
}

// GetForUpdate locks and returns a deposit inside tx.
func (r *Repository) GetForUpdate(ctx context.Context, tx pgx.Tx, depositID DepositID) (Deposit, error) {
	return r.get(ctx, tx, depositID, true)
}

func (r *Repository) get(ctx context.Context, q db.Querier, depositID DepositID, lock bool) (Deposit, error) {
	sql := `SELECT ` + depositColumns + ` FROM deposits WHERE id = $1`
	if lock {
		sql += ` FOR UPDATE`
	}
	d, err := scanDeposit(q.QueryRow(ctx, sql, depositID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Deposit{}, errs.New(errs.CodeNotFound, "deposit not found").WithField("deposit_id", depositID.String())
		}
		return Deposit{}, dbErr("get deposit", err)
	}
	return d, nil
}

// GetByIdempotencyKey looks a deposit up by its idempotency key.
func (r *Repository) GetByIdempotencyKey(ctx context.Context, q db.Querier, key string) (Deposit, bool, error) {
	d, err := scanDeposit(q.QueryRow(ctx, `SELECT `+depositColumns+` FROM deposits WHERE idempotency_key = $1`, key))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Deposit{}, false, nil
		}
		return Deposit{}, false, dbErr("get deposit by key", err)
	}
	return d, true, nil
}

// GetByProviderSession locks and returns the deposit bound to a provider
// session; NOT_FOUND when the session is unknown to this platform.
func (r *Repository) GetByProviderSession(ctx context.Context, tx pgx.Tx, provider, sessionID string) (Deposit, error) {
	if provider == "" || sessionID == "" {
		return Deposit{}, errs.New(errs.CodeValidationFailed, "funding: provider and session id are required")
	}
	d, err := scanDeposit(tx.QueryRow(ctx, `SELECT `+depositColumns+` FROM deposits WHERE provider = $1 AND provider_session_id = $2 FOR UPDATE`, provider, sessionID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Deposit{}, errs.New(errs.CodeNotFound, "deposit not found for provider session").
				WithField("provider", provider).WithField("provider_session_id", sessionID)
		}
		return Deposit{}, dbErr("get deposit by session", err)
	}
	return d, nil
}

// MaxPageSize bounds ListByAccount.
const MaxPageSize = 200

// ListByAccount returns deposits of an account newest first, with an
// opaque cursor for the next page ("" when exhausted).
func (r *Repository) ListByAccount(ctx context.Context, q db.Querier, accountID accounts.AccountID, cursor string, limit int) ([]Deposit, string, error) {
	if limit <= 0 || limit > MaxPageSize {
		limit = MaxPageSize
	}
	after, afterID, hasCursor, err := decodeCursor(cursor)
	if err != nil {
		return nil, "", err
	}
	var rows pgx.Rows
	if hasCursor {
		rows, err = q.Query(ctx, `SELECT `+depositColumns+` FROM deposits WHERE account_id = $1 AND (created_at, id) < ($2, $3)
			ORDER BY created_at DESC, id DESC LIMIT $4`, accountID, after, afterID, limit+1)
	} else {
		rows, err = q.Query(ctx, `SELECT `+depositColumns+` FROM deposits WHERE account_id = $1
			ORDER BY created_at DESC, id DESC LIMIT $2`, accountID, limit+1)
	}
	if err != nil {
		return nil, "", dbErr("list deposits", err)
	}
	defer rows.Close()
	out := make([]Deposit, 0, limit)
	for rows.Next() {
		d, err := scanDeposit(rows)
		if err != nil {
			return nil, "", dbErr("scan deposit", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, "", dbErr("iterate deposits", err)
	}
	next := ""
	if len(out) > limit {
		out = out[:limit]
		last := out[len(out)-1]
		next = encodeCursor(last.CreatedAt, last.ID)
	}
	return out, next, nil
}

func encodeCursor(t time.Time, depositID DepositID) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(t.UTC().UnixNano(), 10) + "|" + depositID.String()))
}

func decodeCursor(s string) (time.Time, DepositID, bool, error) {
	if s == "" {
		return time.Time{}, DepositID{}, false, nil
	}
	invalid := errs.New(errs.CodeValidationFailed, "funding: invalid cursor")
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return time.Time{}, DepositID{}, false, invalid
	}
	parts := strings.SplitN(string(raw), "|", 2)
	if len(parts) != 2 {
		return time.Time{}, DepositID{}, false, invalid
	}
	nanos, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return time.Time{}, DepositID{}, false, invalid
	}
	depositID, err := ParseDepositID(parts[1])
	if err != nil {
		return time.Time{}, DepositID{}, false, invalid
	}
	return time.Unix(0, nanos).UTC(), depositID, true, nil
}

// Transitions returns the transition history of a deposit, oldest first.
func (r *Repository) Transitions(ctx context.Context, q db.Querier, depositID DepositID) ([]Transition, error) {
	rows, err := q.Query(ctx, `SELECT id, deposit_id, from_status, to_status, actor_type, actor_id, reason, coalesce(evidence_ref, ''), coalesce(correlation_id, ''), occurred_at
		FROM deposit_transitions WHERE deposit_id = $1 ORDER BY occurred_at, id`, depositID)
	if err != nil {
		return nil, dbErr("list transitions", err)
	}
	defer rows.Close()
	var out []Transition
	for rows.Next() {
		var t Transition
		var from, to, actorType string
		if err := rows.Scan(&t.ID, &t.DepositID, &from, &to, &actorType, &t.ActorID, &t.Reason, &t.EvidenceRef, &t.CorrelationID, &t.OccurredAt); err != nil {
			return nil, dbErr("scan transition", err)
		}
		t.From, t.To, t.ActorType = Status(from), Status(to), security.ActorType(actorType)
		out = append(out, t)
	}
	return out, rows.Err()
}

// Transition moves a deposit to a new status with no field changes.
func (r *Repository) Transition(ctx context.Context, tx pgx.Tx, depositID DepositID, to Status, ev TransitionEvidence) (Deposit, error) {
	return r.TransitionWithPatch(ctx, tx, depositID, to, ev, Patch{})
}

// TransitionWithPatch moves a deposit to a new status and applies the
// patch in the same UPDATE. It locks the row, checks CanTransition (an
// illegal move is INVALID_STATE_TRANSITION), enforces the evidence
// preconditions of platform-owned states, inserts the immutable
// deposit_transitions row (which migration 00603 requires before the status
// can change), stamps the status timestamp, and writes the outbox and
// audit events.
func (r *Repository) TransitionWithPatch(ctx context.Context, tx pgx.Tx, depositID DepositID, to Status, ev TransitionEvidence, p Patch) (Deposit, error) {
	if tx == nil {
		return Deposit{}, errs.New(errs.CodeInternal, "funding: transition requires a transaction")
	}
	if err := ev.Validate(); err != nil {
		return Deposit{}, err
	}
	if !to.Valid() {
		return Deposit{}, errs.Newf(errs.CodeValidationFailed, "funding: unknown status %q", to)
	}
	cur, err := r.GetForUpdate(ctx, tx, depositID)
	if err != nil {
		return Deposit{}, err
	}
	if !CanTransition(cur.Status, to) {
		return Deposit{}, errs.Newf(errs.CodeInvalidStateTransition, "deposit %s -> %s is not allowed", cur.Status, to).
			WithField("deposit_id", depositID.String()).WithField("from", string(cur.Status)).WithField("to", string(to))
	}
	if err := requireEvidence(cur, to, p); err != nil {
		return Deposit{}, err
	}
	now := r.clk.Now().UTC()
	transitionID := NewTransitionID()
	if _, err := tx.Exec(ctx, `INSERT INTO deposit_transitions (id, deposit_id, from_status, to_status, actor_type, actor_id, reason, evidence_ref, correlation_id, occurred_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NULLIF($8, ''), NULLIF($9, ''), $10)`,
		transitionID, depositID, cur.Status, to, string(ev.ActorType), ev.ActorID, ev.Reason, ev.EvidenceRef, ev.CorrelationID, now); err != nil {
		return Deposit{}, dbErr("insert transition", err)
	}
	sets := []string{"status = $2", to.TimestampColumn() + " = $3"}
	args := []any{depositID, to, now}
	add := func(col string, v any) {
		args = append(args, v)
		sets = append(sets, fmt.Sprintf("%s = $%d", col, len(args)))
	}
	if p.ProviderSessionID != nil {
		add("provider_session_id", *p.ProviderSessionID)
	}
	if p.ProviderRef != nil {
		add("provider_ref", *p.ProviderRef)
	}
	if p.FiatAmountMinor != nil {
		add("fiat_amount_minor", *p.FiatAmountMinor)
	}
	if p.FiatCurrency != nil {
		add("fiat_currency", strings.ToLower(*p.FiatCurrency))
	}
	if p.ExpectedQuantity != nil {
		add("expected_quantity", p.ExpectedQuantity.String())
	}
	if p.ObservedQuantity != nil {
		add("observed_quantity", p.ObservedQuantity.String())
	}
	if p.TxSignature != nil {
		add("tx_signature", *p.TxSignature)
	}
	if p.ChainSlot != nil {
		add("chain_slot", *p.ChainSlot)
	}
	if p.FraudState != nil {
		if !p.FraudState.Valid() {
			return Deposit{}, errs.Newf(errs.CodeValidationFailed, "funding: unknown fraud state %q", *p.FraudState)
		}
		add("fraud_state", string(*p.FraudState))
	}
	if p.ReversibleUntil != nil {
		add("reversible_until", p.ReversibleUntil.UTC())
	}
	if p.BuyingPowerEligible != nil {
		add("buying_power_eligible", *p.BuyingPowerEligible)
	}
	if p.WithdrawalEligible != nil {
		add("withdrawal_eligible", *p.WithdrawalEligible)
	}
	if p.AvailabilityPolicyVersion != nil {
		add("availability_policy_version", *p.AvailabilityPolicyVersion)
	}
	if p.JournalTransactionID != nil {
		add("journal_transaction_id", *p.JournalTransactionID)
	}
	if p.ReversalJournalTransactionID != nil {
		add("reversal_journal_transaction_id", *p.ReversalJournalTransactionID)
	}
	updated, err := scanDeposit(tx.QueryRow(ctx, `UPDATE deposits SET `+strings.Join(sets, ", ")+` WHERE id = $1 RETURNING `+depositColumns, args...))
	if err != nil {
		if db.IsUniqueViolation(err) {
			return Deposit{}, errs.Wrap(err, errs.CodeConflict, "funding: provider session is already bound to another deposit")
		}
		return Deposit{}, dbErr("update deposit", err)
	}
	if err := r.announce(ctx, tx, updated, cur.Status, to, transitionID, ev, now, "funding.deposit.transitioned"); err != nil {
		return Deposit{}, err
	}
	return updated, nil
}

// requireEvidence enforces that platform-owned states carry the evidence
// that justifies them, whether already stored or arriving in the patch.
func requireEvidence(cur Deposit, to Status, p Patch) error {
	missing := func(what string) error {
		return errs.Newf(errs.CodeInvalidStateTransition, "deposit cannot become %s without %s", to, what).
			WithField("deposit_id", cur.ID.String()).WithField("to", string(to)).WithField("missing", what)
	}
	switch to {
	case StatusSessionCreated:
		if cur.ProviderSessionID == "" && (p.ProviderSessionID == nil || *p.ProviderSessionID == "") {
			return missing("a provider session id")
		}
	case StatusSettlementObserved:
		if cur.ObservedQuantity == nil && p.ObservedQuantity == nil {
			return missing("an observed chain quantity")
		}
		if cur.TxSignature == "" && (p.TxSignature == nil || *p.TxSignature == "") {
			return missing("a chain transaction signature")
		}
	case StatusReconciled, StatusAvailable:
		if cur.JournalTransactionID == nil && p.JournalTransactionID == nil {
			return missing("a FUNDING_SETTLED journal transaction")
		}
	case StatusReversed:
		if cur.ReversalJournalTransactionID == nil && p.ReversalJournalTransactionID == nil {
			return missing("a reversal journal transaction")
		}
	}
	return nil
}

// transitionPayload is the outbox and audit payload of a deposit event.
type transitionPayload struct {
	Event                string         `json:"event"`
	DepositID            string         `json:"deposit_id"`
	TransitionID         string         `json:"transition_id,omitempty"`
	AccountID            string         `json:"account_id"`
	Provider             string         `json:"provider"`
	ProviderSessionID    string         `json:"provider_session_id,omitempty"`
	From                 string         `json:"from,omitempty"`
	To                   string         `json:"to"`
	Reason               string         `json:"reason"`
	EvidenceRef          string         `json:"evidence_ref,omitempty"`
	ExpectedAssetID      string         `json:"expected_asset_id"`
	ExpectedQuantity     *string        `json:"expected_quantity,omitempty"`
	ObservedQuantity     *string        `json:"observed_quantity,omitempty"`
	TxSignature          string         `json:"tx_signature,omitempty"`
	JournalTransactionID string         `json:"journal_transaction_id,omitempty"`
	ReversalTransaction  string         `json:"reversal_journal_transaction_id,omitempty"`
	FraudState           string         `json:"fraud_state"`
	BuyingPowerEligible  bool           `json:"buying_power_eligible"`
	WithdrawalEligible   bool           `json:"withdrawal_eligible"`
	OccurredAt           string         `json:"occurred_at"`
	Detail               map[string]any `json:"detail,omitempty"`
}

// announce writes the outbox envelope and the audit event for a deposit
// change. eventName distinguishes creation, transitions and the reversal
// (funding.reversed) on the single registered funding topic.
func (r *Repository) announce(ctx context.Context, tx pgx.Tx, d Deposit, from, to Status, transitionID TransitionID, ev TransitionEvidence, now time.Time, eventName string) error {
	if to == StatusReversed {
		eventName = EventFundingReversed
	}
	payload := transitionPayload{
		Event: eventName, DepositID: d.ID.String(), AccountID: d.AccountID.String(), Provider: d.Provider,
		ProviderSessionID: d.ProviderSessionID, From: string(from), To: string(to), Reason: ev.Reason, EvidenceRef: ev.EvidenceRef,
		ExpectedAssetID: d.ExpectedAssetID.String(), TxSignature: d.TxSignature, FraudState: string(d.FraudState),
		BuyingPowerEligible: d.BuyingPowerEligible, WithdrawalEligible: d.WithdrawalEligible, OccurredAt: now.Format(time.RFC3339Nano),
		Detail: ev.Detail,
	}
	if !transitionID.IsZero() {
		payload.TransitionID = transitionID.String()
	}
	if d.ExpectedQuantity != nil {
		s := d.ExpectedQuantity.String()
		payload.ExpectedQuantity = &s
	}
	if d.ObservedQuantity != nil {
		s := d.ObservedQuantity.String()
		payload.ObservedQuantity = &s
	}
	if d.JournalTransactionID != nil {
		payload.JournalTransactionID = d.JournalTransactionID.String()
	}
	if d.ReversalJournalTransactionID != nil {
		payload.ReversalTransaction = d.ReversalJournalTransactionID.String()
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return errs.Wrap(err, errs.CodeInternal, "funding: encode event")
	}
	dedup := "deposit:" + d.ID.String() + ":" + eventName
	if !transitionID.IsZero() {
		dedup = "deposit_transition:" + transitionID.String()
	}
	env := event.Envelope{
		ID: event.NewEventID().String(), Type: string(event.TopicFundingDepositTransitioned), SchemaVersion: 1, Source: "funding",
		AggregateType: event.AggregateDeposit, AggregateID: d.ID.String(), CorrelationID: ev.CorrelationID,
		OccurredAt: now, DedupKey: dedup, Headers: map[string]string{"event": eventName}, Payload: body,
	}
	if err := r.outbox.Enqueue(ctx, tx, string(event.TopicFundingDepositTransitioned), env); err != nil {
		return err
	}
	_, err = r.audit.Append(ctx, tx, audit.Event{
		Stream: audit.AccountStream(d.AccountID.String()), ActorType: string(ev.ActorType), ActorID: ev.ActorID,
		Action: eventName, ResourceType: "deposit", ResourceID: d.ID.String(),
		RequestID: ev.RequestID, CorrelationID: ev.CorrelationID, Reason: ev.Reason, EvidenceRef: ev.EvidenceRef,
		Payload: body, OccurredAt: now,
	})
	return err
}

// EventFundingReversed is the event name carried in the outbox header and
// payload (and the audit action) of a reversal.
const EventFundingReversed = "funding.reversed"

// SetWithdrawalEligible flips withdrawal_eligible on an AVAILABLE deposit
// whose reversible window has passed and whose fraud state allows it. It is
// not a status change, so no transition row is needed, but it is audited
// and announced. changed is false when the row did not qualify.
func (r *Repository) SetWithdrawalEligible(ctx context.Context, tx pgx.Tx, depositID DepositID, now time.Time, ev TransitionEvidence) (dep Deposit, changed bool, err error) {
	if err := ev.Validate(); err != nil {
		return Deposit{}, false, err
	}
	d, err := scanDeposit(tx.QueryRow(ctx, `UPDATE deposits SET withdrawal_eligible = true
		WHERE id = $1 AND status = 'AVAILABLE' AND NOT withdrawal_eligible AND reversible_until IS NOT NULL AND reversible_until <= $2
		  AND fraud_state IN ('NONE', 'CLEARED')
		RETURNING `+depositColumns, depositID, now.UTC()))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Deposit{}, false, nil
		}
		return Deposit{}, false, dbErr("set withdrawal eligible", err)
	}
	if err := r.announce(ctx, tx, d, d.Status, d.Status, TransitionID{}, ev, r.clk.Now().UTC(), "funding.deposit.withdrawal_eligible"); err != nil {
		return Deposit{}, false, err
	}
	return d, true, nil
}

// LockDue locks up to limit deposits in the given statuses created before
// cutoff (SKIP LOCKED so concurrent sweepers do not collide).
func (r *Repository) LockDue(ctx context.Context, tx pgx.Tx, statuses []Status, cutoff time.Time, limit int) ([]DepositID, error) {
	if limit <= 0 {
		limit = 100
	}
	names := make([]string, len(statuses))
	for i, s := range statuses {
		names[i] = string(s)
	}
	rows, err := tx.Query(ctx, `SELECT id FROM deposits WHERE status = ANY($1) AND created_at < $2 ORDER BY created_at LIMIT $3 FOR UPDATE SKIP LOCKED`,
		names, cutoff.UTC(), limit)
	if err != nil {
		return nil, dbErr("lock due deposits", err)
	}
	defer rows.Close()
	var out []DepositID
	for rows.Next() {
		var depositID DepositID
		if err := rows.Scan(&depositID); err != nil {
			return nil, dbErr("scan deposit id", err)
		}
		out = append(out, depositID)
	}
	return out, rows.Err()
}

// LockWithdrawalCandidates locks AVAILABLE deposits whose reversible window
// has passed and that are not yet withdrawal-eligible.
func (r *Repository) LockWithdrawalCandidates(ctx context.Context, tx pgx.Tx, now time.Time, limit int) ([]DepositID, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := tx.Query(ctx, `SELECT id FROM deposits WHERE status = 'AVAILABLE' AND NOT withdrawal_eligible
		AND reversible_until IS NOT NULL AND reversible_until <= $1 AND fraud_state IN ('NONE', 'CLEARED')
		ORDER BY reversible_until LIMIT $2 FOR UPDATE SKIP LOCKED`, now.UTC(), limit)
	if err != nil {
		return nil, dbErr("lock withdrawal candidates", err)
	}
	defer rows.Close()
	var out []DepositID
	for rows.Next() {
		var depositID DepositID
		if err := rows.Scan(&depositID); err != nil {
			return nil, dbErr("scan deposit id", err)
		}
		out = append(out, depositID)
	}
	return out, rows.Err()
}
