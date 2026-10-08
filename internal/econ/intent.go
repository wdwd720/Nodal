package econ

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/event"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
)

// Intent is an economic_intents row: a typed request to cause an economic
// effect. RECEIVED means recorded, never allowed.
type Intent struct {
	ID           IntentID
	AccountID    accounts.AccountID
	Kind         IntentKind
	Mode         gates.Mode
	Capability   gates.Capability
	ActorType    security.ActorType
	ActorID      string
	AgentID      string
	StrategyRef  string
	DelegationID string
	InstrumentID string
	AssetID      *assets.AssetID
	Quantity     *money.Quantity
	NotionalUSD  *money.USD
	// Terms are the kind-specific typed terms, as canonical JSON. The owning
	// subsystem decodes them into its own type.
	Terms          json.RawMessage
	ContentHash    []byte
	IdempotencyKey string
	Status         IntentStatus
	RejectionCode  string
	CorrelationID  string
	CreatedAt      time.Time
	UpdatedAt      time.Time
	TerminalAt     *time.Time
}

// Validate checks structure without touching the database.
func (in Intent) Validate() error {
	var problems []string
	if in.AccountID.IsZero() {
		problems = append(problems, "account_id required")
	}
	if !in.Kind.Valid() {
		problems = append(problems, fmt.Sprintf("unknown intent kind %q", in.Kind))
	}
	persistable := false
	for _, m := range PersistableModes() {
		if m == in.Mode {
			persistable = true
		}
	}
	if !persistable {
		problems = append(problems, fmt.Sprintf("mode %q is not PAPER, SANDBOX or DEVNET; no advanced intent can be LIVE", in.Mode))
	}
	if _, ok := gates.LookupAdvanced(in.Capability); !ok {
		problems = append(problems, fmt.Sprintf("capability %q is not an advanced capability", in.Capability))
	}
	if !in.ActorType.Valid() || strings.TrimSpace(in.ActorID) == "" {
		problems = append(problems, "actor type and id required")
	}
	if (in.ActorType == security.ActorAgent) != (in.AgentID != "") {
		problems = append(problems, "an agent intent names its agent, and only an agent intent does")
	}
	for name, v := range map[string]string{"agent_id": in.AgentID, "delegation_id": in.DelegationID, "instrument_id": in.InstrumentID} {
		if v != "" {
			if _, err := id.ParseAny(v); err != nil {
				problems = append(problems, name+" must be a canonical uuid")
			}
		}
	}
	if in.Quantity != nil && !in.Quantity.IsPositive() {
		problems = append(problems, "quantity must be positive when present")
	}
	if in.NotionalUSD != nil && in.NotionalUSD.IsNegative() {
		problems = append(problems, "notional must not be negative")
	}
	if k := strings.TrimSpace(in.IdempotencyKey); k == "" || k != in.IdempotencyKey || len(k) > 256 {
		problems = append(problems, "idempotency key must be 1..256 bytes without surrounding whitespace")
	}
	if len(in.Terms) > 0 {
		var obj map[string]any
		if err := json.Unmarshal(in.Terms, &obj); err != nil || obj == nil {
			problems = append(problems, "terms must be a JSON object")
		}
	}
	if len(problems) > 0 {
		return errs.New(errs.CodeValidationFailed, "invalid economic intent").WithField("problems", problems)
	}
	return nil
}

// canonicalIntent is the semantic content an intent's hash covers: everything
// the requester stated, nothing the server assigned.
type canonicalIntent struct {
	AccountID    string          `json:"account_id"`
	Kind         string          `json:"kind"`
	Mode         string          `json:"mode"`
	Capability   string          `json:"capability"`
	ActorType    string          `json:"actor_type"`
	ActorID      string          `json:"actor_id"`
	AgentID      string          `json:"agent_id"`
	StrategyRef  string          `json:"strategy_ref"`
	DelegationID string          `json:"delegation_id"`
	InstrumentID string          `json:"instrument_id"`
	AssetID      string          `json:"asset_id"`
	Quantity     string          `json:"quantity"`
	NotionalUSD  string          `json:"notional_usd"`
	Terms        json.RawMessage `json:"terms"`
}

// ComputeContentHash returns the sha256 of the intent's canonical semantic
// content. Two intents with the same hash ask for the same thing.
func (in Intent) ComputeContentHash() ([]byte, error) {
	c := canonicalIntent{
		AccountID: in.AccountID.String(), Kind: string(in.Kind), Mode: string(in.Mode), Capability: string(in.Capability),
		ActorType: string(in.ActorType), ActorID: in.ActorID, AgentID: strings.ToLower(in.AgentID),
		StrategyRef: in.StrategyRef, DelegationID: strings.ToLower(in.DelegationID), InstrumentID: strings.ToLower(in.InstrumentID),
	}
	if in.AssetID != nil {
		c.AssetID = in.AssetID.String()
	}
	if in.Quantity != nil {
		c.Quantity = in.Quantity.String()
	}
	if in.NotionalUSD != nil {
		c.NotionalUSD = in.NotionalUSD.String()
	}
	terms := in.Terms
	if len(terms) == 0 {
		terms = json.RawMessage(`{}`)
	}
	// Terms are re-encoded canonically so key order and whitespace in the
	// request cannot change the hash.
	var decoded any
	if err := json.Unmarshal(terms, &decoded); err != nil {
		return nil, errs.Wrap(err, errs.CodeValidationFailed, "econ: terms are not valid JSON")
	}
	canon, err := audit.CanonicalJSON(decoded)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeValidationFailed, "econ: terms are not canonically encodable (money must be a string)")
	}
	c.Terms = canon
	b, err := audit.CanonicalJSON(c)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "econ: encode intent")
	}
	sum := sha256.Sum256(b)
	return sum[:], nil
}

// IntentStore persists economic intents in the caller's transaction.
type IntentStore struct {
	clk    clock.Clock
	outbox *event.Outbox
	audit  audit.Writer
}

// NewIntentStore wires an IntentStore.
func NewIntentStore(clk clock.Clock, outbox *event.Outbox, aud audit.Writer) *IntentStore {
	if clk == nil || outbox == nil || aud == nil {
		panic("econ: NewIntentStore requires a clock, an outbox and an audit writer")
	}
	return &IntentStore{clk: clk, outbox: outbox, audit: aud}
}

const intentColumns = `id, account_id, kind, mode, capability, actor_type, actor_id, coalesce(agent_id::text,''),
	coalesce(strategy_ref,''), coalesce(delegation_id::text,''), coalesce(instrument_id::text,''), asset_id,
	quantity::text, notional_usd_minor, terms, content_hash, idempotency_key, status, coalesce(rejection_code,''),
	coalesce(correlation_id,''), created_at, updated_at, terminal_at`

func scanIntent(row pgx.Row) (Intent, error) {
	var in Intent
	var qty *string
	var notional *int64
	var assetID *assets.AssetID
	if err := row.Scan(&in.ID, &in.AccountID, &in.Kind, &in.Mode, &in.Capability, &in.ActorType, &in.ActorID,
		&in.AgentID, &in.StrategyRef, &in.DelegationID, &in.InstrumentID, &assetID, &qty, &notional, &in.Terms,
		&in.ContentHash, &in.IdempotencyKey, &in.Status, &in.RejectionCode, &in.CorrelationID,
		&in.CreatedAt, &in.UpdatedAt, &in.TerminalAt); err != nil {
		return Intent{}, err
	}
	in.AssetID = assetID
	if qty != nil {
		q, err := money.ScanQuantity(*qty)
		if err != nil {
			return Intent{}, err
		}
		in.Quantity = &q
	}
	if notional != nil {
		u := money.USDFromMinor(*notional)
		in.NotionalUSD = &u
	}
	in.CreatedAt = in.CreatedAt.UTC()
	in.UpdatedAt = in.UpdatedAt.UTC()
	return in, nil
}

// Submit records an intent in RECEIVED. It is idempotent on (account,
// idempotency key): the same key with the same content returns the original
// intent and existing=true; the same key with different content fails with
// INVALID_IDEMPOTENCY_REUSE.
func (s *IntentStore) Submit(ctx context.Context, tx pgx.Tx, in Intent) (Intent, bool, error) {
	if tx == nil {
		return Intent{}, false, errs.New(errs.CodeInternal, "econ: Submit requires a transaction")
	}
	if err := in.Validate(); err != nil {
		return Intent{}, false, err
	}
	hash, err := in.ComputeContentHash()
	if err != nil {
		return Intent{}, false, err
	}
	if len(in.Terms) == 0 {
		in.Terms = json.RawMessage(`{}`)
	}
	var qty any
	if in.Quantity != nil {
		qty = in.Quantity.String()
	}
	var notional any
	if in.NotionalUSD != nil {
		notional = in.NotionalUSD.Minor()
	}
	now := s.clk.Now().UTC()
	iid := NewIntentID()
	row := tx.QueryRow(ctx, `INSERT INTO economic_intents
		(id, account_id, kind, mode, capability, actor_type, actor_id, agent_id, strategy_ref, delegation_id,
		 instrument_id, asset_id, quantity, notional_usd_minor, terms, content_hash, idempotency_key, status,
		 correlation_id, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,NULLIF($8,'')::uuid,NULLIF($9,''),NULLIF($10,'')::uuid,
		        NULLIF($11,'')::uuid,$12,$13::numeric,$14,$15,$16,$17,'RECEIVED',NULLIF($18,''),$19,$19)
		ON CONFLICT (account_id, idempotency_key) DO NOTHING
		RETURNING `+intentColumns,
		iid, in.AccountID, in.Kind, in.Mode, in.Capability, in.ActorType, in.ActorID, in.AgentID, in.StrategyRef,
		in.DelegationID, in.InstrumentID, in.AssetID, qty, notional, []byte(in.Terms), hash, in.IdempotencyKey,
		in.CorrelationID, now)
	created, err := scanIntent(row)
	if err == nil {
		if err := s.announce(ctx, tx, created, "", StatusReceived, Actor{Type: in.ActorType, ID: in.ActorID}, "intent received"); err != nil {
			return Intent{}, false, err
		}
		return created, false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Intent{}, false, dbErr("submit intent", err)
	}
	existing, err := scanIntent(tx.QueryRow(ctx, `SELECT `+intentColumns+` FROM economic_intents
		WHERE account_id = $1 AND idempotency_key = $2`, in.AccountID, in.IdempotencyKey))
	if err != nil {
		return Intent{}, false, dbErr("load intent by key", err)
	}
	if !bytes.Equal(existing.ContentHash, hash) {
		return Intent{}, false, errs.New(errs.CodeInvalidIdempotencyReuse,
			"econ: this idempotency key was already used for a different intent").
			WithField("intent_id", existing.ID.String())
	}
	return existing, true, nil
}

// Transition moves an intent to a new status, writing the transition row,
// the outbox event and the audit event in the same transaction.
func (s *IntentStore) Transition(ctx context.Context, tx pgx.Tx, iid IntentID, to IntentStatus, actor Actor, reason, rejectionCode string) (Intent, error) {
	if !to.Valid() {
		return Intent{}, errs.Newf(errs.CodeValidationFailed, "unknown intent status %q", to)
	}
	if actor.Type == security.ActorAgent {
		return Intent{}, errs.New(errs.CodeForbidden, "econ: an agent does not move an intent through its lifecycle")
	}
	if !actor.Type.Valid() || strings.TrimSpace(actor.ID) == "" {
		return Intent{}, errs.New(errs.CodeValidationFailed, "econ: actor required")
	}
	cur, err := scanIntent(tx.QueryRow(ctx, `SELECT `+intentColumns+` FROM economic_intents WHERE id = $1 FOR UPDATE`, iid))
	if errors.Is(err, pgx.ErrNoRows) {
		return Intent{}, errs.New(errs.CodeNotFound, "economic intent not found").WithField("intent_id", iid.String())
	}
	if err != nil {
		return Intent{}, dbErr("lock intent", err)
	}
	if cur.Status == to {
		return cur, nil
	}
	if !CanTransitionIntent(cur.Status, to) {
		return Intent{}, errs.Newf(errs.CodeInvalidStateTransition, "economic intent %s cannot move %s -> %s", iid, cur.Status, to).
			WithField("intent_id", iid.String())
	}
	if to == StatusRejected && strings.TrimSpace(rejectionCode) == "" {
		return Intent{}, errs.New(errs.CodeValidationFailed, "econ: a rejection needs a stable rejection code")
	}
	now := s.clk.Now().UTC()
	if _, err := tx.Exec(ctx, `INSERT INTO economic_intent_transitions
		(id, intent_id, from_status, to_status, actor_type, actor_id, reason, correlation_id, occurred_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,NULLIF($8,''),$9)`,
		id.New[id.Any](), iid, cur.Status, to, actor.Type, actor.ID, reasonOr(reason, string(to)), cur.CorrelationID, now); err != nil {
		return Intent{}, dbErr("record intent transition", err)
	}
	var terminal any
	if to.Terminal() {
		terminal = now
	}
	out, err := scanIntent(tx.QueryRow(ctx, `UPDATE economic_intents
		SET status = $2, rejection_code = coalesce(NULLIF($3,''), rejection_code), terminal_at = coalesce($4, terminal_at)
		WHERE id = $1 RETURNING `+intentColumns, iid, to, rejectionCode, terminal))
	if err != nil {
		return Intent{}, dbErr("update intent", err)
	}
	if err := s.announce(ctx, tx, out, cur.Status, to, actor, reasonOr(reason, string(to))); err != nil {
		return Intent{}, err
	}
	return out, nil
}

// Get returns one intent.
func (s *IntentStore) Get(ctx context.Context, q db.Querier, iid IntentID) (Intent, error) {
	in, err := scanIntent(q.QueryRow(ctx, `SELECT `+intentColumns+` FROM economic_intents WHERE id = $1`, iid))
	if errors.Is(err, pgx.ErrNoRows) {
		return Intent{}, errs.New(errs.CodeNotFound, "economic intent not found").WithField("intent_id", iid.String())
	}
	if err != nil {
		return Intent{}, dbErr("get intent", err)
	}
	return in, nil
}

// ListForAccount pages an account's intents, newest first. The cursor is the
// id of the last intent of the previous page.
func (s *IntentStore) ListForAccount(ctx context.Context, q db.Querier, account accounts.AccountID, cursor string, limit int) ([]Intent, string, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var rows pgx.Rows
	var err error
	if cursor == "" {
		rows, err = q.Query(ctx, `SELECT `+intentColumns+` FROM economic_intents WHERE account_id = $1
			ORDER BY created_at DESC, id DESC LIMIT $2`, account, limit+1)
	} else {
		cid, perr := ParseIntentID(cursor)
		if perr != nil {
			return nil, "", errs.New(errs.CodeValidationFailed, "invalid cursor")
		}
		rows, err = q.Query(ctx, `SELECT `+intentColumns+` FROM economic_intents WHERE account_id = $1
			AND (created_at, id) < (SELECT created_at, id FROM economic_intents WHERE id = $2)
			ORDER BY created_at DESC, id DESC LIMIT $3`, account, cid, limit+1)
	}
	if err != nil {
		return nil, "", dbErr("list intents", err)
	}
	defer rows.Close()
	var out []Intent
	for rows.Next() {
		in, err := scanIntent(rows)
		if err != nil {
			return nil, "", dbErr("scan intent", err)
		}
		out = append(out, in)
	}
	if err := rows.Err(); err != nil {
		return nil, "", dbErr("list intents", err)
	}
	next := ""
	if len(out) > limit {
		out = out[:limit]
		next = out[len(out)-1].ID.String()
	}
	return out, next, nil
}

type intentEvent struct {
	IntentID      string `json:"intent_id"`
	AccountID     string `json:"account_id"`
	Kind          string `json:"kind"`
	Mode          string `json:"mode"`
	Capability    string `json:"capability"`
	From          string `json:"from_status,omitempty"`
	To            string `json:"to_status"`
	RejectionCode string `json:"rejection_code,omitempty"`
	Reason        string `json:"reason"`
}

func (s *IntentStore) announce(ctx context.Context, tx pgx.Tx, in Intent, from, to IntentStatus, actor Actor, reason string) error {
	body, err := json.Marshal(intentEvent{
		IntentID: in.ID.String(), AccountID: in.AccountID.String(), Kind: string(in.Kind), Mode: string(in.Mode),
		Capability: string(in.Capability), From: string(from), To: string(to), RejectionCode: in.RejectionCode, Reason: reason,
	})
	if err != nil {
		return errs.Wrap(err, errs.CodeInternal, "econ: encode intent event")
	}
	now := s.clk.Now().UTC()
	env := event.Envelope{
		ID: event.NewEventID().String(), Type: string(event.TopicEconIntentTransitioned),
		SchemaVersion: event.TopicEconIntentTransitioned.Version(), Source: "econ",
		AggregateType: event.AggregateEconomicIntent, AggregateID: in.ID.String(),
		CorrelationID: in.CorrelationID, OccurredAt: now,
		DedupKey: in.ID.String() + ":" + string(to), Payload: body,
	}
	if err := s.outbox.Enqueue(ctx, tx, string(event.TopicEconIntentTransitioned), env); err != nil {
		return err
	}
	_, err = s.audit.Append(ctx, tx, audit.Event{
		Stream: audit.AccountStream(in.AccountID.String()), ActorType: string(actor.Type), ActorID: actor.ID,
		Action: "econ.intent." + strings.ToLower(string(to)), ResourceType: "economic_intent", ResourceID: in.ID.String(),
		CorrelationID: in.CorrelationID, Reason: reason, Payload: body, OccurredAt: now,
	})
	return err
}
