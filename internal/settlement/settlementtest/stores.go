package settlementtest

import (
	"context"
	"encoding/json"
	"sort"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/execution"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/positions"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/settlement"
)

// MemDB is a settlement.Transactor that runs fn with a nil transaction:
// the in-memory stores commit immediately, which is exactly the property
// crash tests need (state persisted before a crash stays persisted).
type MemDB struct {
	mu  sync.Mutex
	Txs int
}

// InTx implements Transactor.
func (m *MemDB) InTx(ctx context.Context, _ db.TxOptions, fn func(ctx context.Context, tx pgx.Tx) error) error {
	m.mu.Lock()
	m.Txs++
	m.mu.Unlock()
	return fn(ctx, nil)
}

// --- plans -------------------------------------------------------------------

// MemPlans is an in-memory settlement.PlanStore.
type MemPlans struct {
	mu    sync.Mutex
	clk   clock.Clock
	plans map[settlement.PlanID]*settlement.Plan
}

// NewMemPlans returns an empty store.
func NewMemPlans(clk clock.Clock) *MemPlans {
	return &MemPlans{clk: clk, plans: map[settlement.PlanID]*settlement.Plan{}}
}

// Put stores a plan (replacing any previous copy).
func (m *MemPlans) Put(p settlement.Plan) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := clonePlan(p)
	m.plans[p.ID] = &cp
}

func clonePlan(p settlement.Plan) settlement.Plan {
	cp := p
	cp.Steps = make([]settlement.Step, len(p.Steps))
	for i, s := range p.Steps {
		cs := s
		cs.DependsOn = append([]settlement.StepID(nil), s.DependsOn...)
		cs.EvidenceInputs = append(json.RawMessage(nil), s.EvidenceInputs...)
		cs.EvidenceOutput = append(json.RawMessage(nil), s.EvidenceOutput...)
		cp.Steps[i] = cs
	}
	cp.NoPlanReasonCodes = append([]string(nil), p.NoPlanReasonCodes...)
	cp.Hash = append([]byte(nil), p.Hash...)
	pv := settlement.PolicyVersions{}
	for k, v := range p.PolicyVersions {
		pv[k] = v
	}
	cp.PolicyVersions = pv
	return cp
}

// Get implements PlanStore.
func (m *MemPlans) Get(_ context.Context, _ db.Querier, planID settlement.PlanID) (settlement.Plan, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.plans[planID]
	if !ok {
		return settlement.Plan{}, errs.New(errs.CodeNotFound, "plan not found")
	}
	return clonePlan(*p), nil
}

// SetStatus implements PlanStore.
func (m *MemPlans) SetStatus(_ context.Context, _ pgx.Tx, planID settlement.PlanID, from, to settlement.PlanStatus, _ string) (settlement.Plan, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.plans[planID]
	if !ok {
		return settlement.Plan{}, errs.New(errs.CodeNotFound, "plan not found")
	}
	if !settlement.CanTransitionPlan(from, to) {
		return settlement.Plan{}, errs.Newf(errs.CodeInvalidStateTransition, "plan %s -> %s", from, to)
	}
	if p.Status != from {
		return settlement.Plan{}, errs.New(errs.CodeConflict, "plan status changed concurrently")
	}
	p.Status = to
	if to.Terminal() && p.FinishedAt == nil {
		t := m.clk.Now()
		p.FinishedAt = &t
	}
	return clonePlan(*p), nil
}

// MarkStep implements PlanStore.
func (m *MemPlans) MarkStep(_ context.Context, _ pgx.Tx, stepID settlement.StepID, from, to settlement.StepState, out settlement.StepOutcome) (settlement.Step, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, p := range m.plans {
		for i := range p.Steps {
			s := &p.Steps[i]
			if s.ID != stepID {
				continue
			}
			if s.State != from {
				return settlement.Step{}, errs.New(errs.CodeConflict, "step state changed concurrently").WithField("expected", string(from)).WithField("actual", string(s.State))
			}
			now := m.clk.Now()
			s.State = to
			if out.EvidenceOutput != nil {
				b, err := json.Marshal(out.EvidenceOutput)
				if err != nil {
					return settlement.Step{}, err
				}
				s.EvidenceOutput = b
			}
			s.LastError = out.LastError
			switch to {
			case settlement.StepRunning:
				s.Attempts++
				s.StartedAt = &now
			case settlement.StepSucceeded, settlement.StepFailed, settlement.StepUnknown, settlement.StepSkipped, settlement.StepCompensated:
				s.FinishedAt = &now
			}
			cs := *s
			cs.EvidenceOutput = append(json.RawMessage(nil), s.EvidenceOutput...)
			return cs, nil
		}
	}
	return settlement.Step{}, errs.New(errs.CodeNotFound, "step not found")
}

// ResetSteps implements PlanStore.
func (m *MemPlans) ResetSteps(_ context.Context, _ pgx.Tx, planID settlement.PlanID, fromSeq int32) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.plans[planID]
	if !ok {
		return errs.New(errs.CodeNotFound, "plan not found")
	}
	for i := range p.Steps {
		s := &p.Steps[i]
		if s.Seq >= fromSeq && s.State != settlement.StepPending {
			s.State = settlement.StepPending
			s.EvidenceOutput, s.LastError, s.StartedAt, s.FinishedAt = nil, "", nil, nil
		}
	}
	return nil
}

// --- orders and fills ----------------------------------------------------------

// MemOrders is an in-memory settlement.OrderStore with the same rules as
// execution.Repository: the PART 47 table, fill idempotency under (venue,
// external_fill_id), cumulative quantities and set-once markers.
type MemOrders struct {
	mu          sync.Mutex
	clk         clock.Clock
	orders      map[execution.OrderID]*execution.Order
	fills       map[execution.FillID]*execution.Fill
	byExternal  map[string]execution.FillID
	Transitions []execution.Transition
}

// NewMemOrders returns an empty store.
func NewMemOrders(clk clock.Clock) *MemOrders {
	return &MemOrders{clk: clk, orders: map[execution.OrderID]*execution.Order{}, fills: map[execution.FillID]*execution.Fill{}, byExternal: map[string]execution.FillID{}}
}

// Create implements OrderStore.
func (m *MemOrders) Create(_ context.Context, _ pgx.Tx, o execution.Order) (execution.Order, error) {
	if o.Status == "" {
		o.Status = execution.OrderCreated
	}
	if err := o.Validate(); err != nil {
		return execution.Order{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, x := range m.orders {
		if x.IntentID == o.IntentID {
			return execution.Order{}, errs.New(errs.CodeConflict, "order exists for intent")
		}
	}
	now := m.clk.Now()
	o.CreatedAt, o.UpdatedAt = now, now
	cp := o
	m.orders[o.ID] = &cp
	return o, nil
}

// Get implements OrderStore.
func (m *MemOrders) Get(_ context.Context, _ db.Querier, orderID execution.OrderID) (execution.Order, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.orders[orderID]
	if !ok {
		return execution.Order{}, errs.New(errs.CodeNotFound, "order not found")
	}
	return *o, nil
}

// GetByPlan implements OrderStore.
func (m *MemOrders) GetByPlan(_ context.Context, _ db.Querier, planID string) (execution.Order, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, o := range m.orders {
		if o.PlanID == planID {
			return *o, nil
		}
	}
	return execution.Order{}, errs.New(errs.CodeNotFound, "order not found")
}

// Orders returns every order.
func (m *MemOrders) Orders() []execution.Order {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]execution.Order, 0, len(m.orders))
	for _, o := range m.orders {
		out = append(out, *o)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// Transition implements OrderStore.
func (m *MemOrders) Transition(_ context.Context, _ pgx.Tx, orderID execution.OrderID, to execution.OrderStatus, ev execution.TransitionEvidence) (execution.Order, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.transitionLocked(orderID, to, ev)
}

func (m *MemOrders) transitionLocked(orderID execution.OrderID, to execution.OrderStatus, ev execution.TransitionEvidence) (execution.Order, error) {
	o, ok := m.orders[orderID]
	if !ok {
		return execution.Order{}, errs.New(errs.CodeNotFound, "order not found")
	}
	if !execution.CanTransition(o.Status, to) {
		return execution.Order{}, errs.Newf(errs.CodeInvalidStateTransition, "order is %s; %s -> %s is not allowed", o.Status, o.Status, to).
			WithField("from", string(o.Status)).WithField("to", string(to))
	}
	if to == execution.OrderRejected && ev.RejectionCode == "" {
		return execution.Order{}, errs.New(errs.CodeValidationFailed, "REJECTED requires a rejection code")
	}
	now := m.clk.Now()
	m.Transitions = append(m.Transitions, execution.Transition{ID: execution.NewTransitionID(), OrderID: orderID, From: o.Status, To: to, ActorType: ev.ActorType, ActorID: ev.ActorID, Reason: ev.Reason, EvidenceRef: ev.EvidenceRef, OccurredAt: now})
	o.Status = to
	if ev.RejectionCode != "" {
		o.RejectionCode = ev.RejectionCode
	}
	if to.Terminal() && o.TerminalAt == nil {
		o.TerminalAt = &now
	}
	o.UpdatedAt = now
	return *o, nil
}

// RecordFill implements OrderStore.
func (m *MemOrders) RecordFill(_ context.Context, _ pgx.Tx, f execution.Fill) (execution.Fill, error) {
	if err := f.Validate(); err != nil {
		return execution.Fill{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	key := f.Venue + "/" + f.ExternalFillID
	if existingID, ok := m.byExternal[key]; ok {
		ex := *m.fills[existingID]
		if ex.OrderID != f.OrderID {
			return execution.Fill{}, errs.New(errs.CodeConflict, "external fill id belongs to another order")
		}
		ex.Existing = true
		return ex, nil
	}
	o, ok := m.orders[f.OrderID]
	if !ok {
		return execution.Fill{}, errs.New(errs.CodeNotFound, "order not found")
	}
	if o.InputAssetID != f.InputAssetID || o.OutputAssetID != f.OutputAssetID {
		return execution.Fill{}, errs.New(errs.CodeReconciliationRequired, "fill assets do not match the order")
	}
	filledIn := o.FilledInputQuantity.Add(f.InputQuantity)
	next, err := execution.NextStatusAfterFill(o.Status, filledIn, o.InputQuantity)
	if err != nil {
		if errs.HasCode(err, errs.CodeInvalidStateTransition) {
			return execution.Fill{}, errs.Wrap(err, errs.CodeReconciliationRequired, "fill for an order that does not accept fills")
		}
		return execution.Fill{}, err
	}
	now := m.clk.Now()
	f.ReceivedAt, f.CreatedAt = now, now
	cp := f
	m.fills[f.ID] = &cp
	m.byExternal[key] = f.ID
	o.FilledInputQuantity = filledIn
	o.FilledOutputQuantity = o.FilledOutputQuantity.Add(f.OutputQuantity)
	if next != o.Status {
		if _, err := m.transitionLocked(o.ID, next, execution.TransitionEvidence{Reason: "fill " + f.ExternalFillID, CausationID: f.ID.String()}); err != nil {
			return execution.Fill{}, err
		}
	}
	return f, nil
}

// ListFills implements OrderStore.
func (m *MemOrders) ListFills(_ context.Context, _ db.Querier, orderID execution.OrderID) ([]execution.Fill, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []execution.Fill
	for _, f := range m.fills {
		if f.OrderID == orderID {
			out = append(out, *f)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].ObservedAt.Equal(out[j].ObservedAt) {
			return out[i].ObservedAt.Before(out[j].ObservedAt)
		}
		return out[i].ID.String() < out[j].ID.String()
	})
	return out, nil
}

// FillCount returns the number of distinct fills recorded.
func (m *MemOrders) FillCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.fills)
}

// MarkFillPosted implements OrderStore (set-once).
func (m *MemOrders) MarkFillPosted(_ context.Context, _ pgx.Tx, fillID execution.FillID, journalTxID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	f, ok := m.fills[fillID]
	if !ok {
		return errs.New(errs.CodeNotFound, "fill not found")
	}
	if f.JournalTransactionID != "" {
		if f.JournalTransactionID == journalTxID {
			return nil
		}
		return errs.New(errs.CodeFillImmutable, "fill already posted")
	}
	f.JournalTransactionID = journalTxID
	return nil
}

// MarkPositionApplied implements OrderStore (set-once).
func (m *MemOrders) MarkPositionApplied(_ context.Context, _ pgx.Tx, fillID execution.FillID, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	f, ok := m.fills[fillID]
	if !ok {
		return errs.New(errs.CodeNotFound, "fill not found")
	}
	if f.PositionAppliedAt != nil {
		return nil
	}
	if at.IsZero() {
		at = m.clk.Now()
	}
	f.PositionAppliedAt = &at
	return nil
}

// --- attempts -------------------------------------------------------------------

// MemAttempts is an in-memory settlement.AttemptStore.
type MemAttempts struct {
	mu       sync.Mutex
	clk      clock.Clock
	orders   *MemOrders
	attempts map[execution.AttemptID]*execution.Attempt
}

// NewMemAttempts returns an empty store over orders.
func NewMemAttempts(clk clock.Clock, orders *MemOrders) *MemAttempts {
	return &MemAttempts{clk: clk, orders: orders, attempts: map[execution.AttemptID]*execution.Attempt{}}
}

// Create implements AttemptStore.
func (m *MemAttempts) Create(ctx context.Context, _ pgx.Tx, a execution.Attempt) (execution.Attempt, error) {
	if a.Status == "" {
		a.Status = execution.AttemptBuilt
	}
	if err := a.Validate(); err != nil {
		return execution.Attempt{}, err
	}
	o, err := m.orders.Get(ctx, nil, a.OrderID)
	if err != nil {
		return execution.Attempt{}, err
	}
	if o.Status.Terminal() {
		return execution.Attempt{}, errs.New(errs.CodeInvalidStateTransition, "order is terminal")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var maxNo int32
	for _, x := range m.attempts {
		if x.OrderID == a.OrderID && x.AttemptNo > maxNo {
			maxNo = x.AttemptNo
		}
	}
	a.AttemptNo = maxNo + 1
	now := m.clk.Now()
	a.CreatedAt, a.UpdatedAt = now, now
	cp := a
	m.attempts[a.ID] = &cp
	return a, nil
}

// Get implements AttemptStore.
func (m *MemAttempts) Get(_ context.Context, _ db.Querier, attemptID execution.AttemptID) (execution.Attempt, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.attempts[attemptID]
	if !ok {
		return execution.Attempt{}, errs.New(errs.CodeNotFound, "attempt not found")
	}
	return *a, nil
}

// Update implements AttemptStore.
func (m *MemAttempts) Update(_ context.Context, _ pgx.Tx, attemptID execution.AttemptID, p execution.AttemptPatch) (execution.Attempt, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.attempts[attemptID]
	if !ok {
		return execution.Attempt{}, errs.New(errs.CodeNotFound, "attempt not found")
	}
	if p.Status != nil && *p.Status != a.Status {
		if !execution.CanTransitionAttempt(a.Status, *p.Status) {
			return execution.Attempt{}, errs.Newf(errs.CodeInvalidStateTransition, "attempt %s -> %s", a.Status, *p.Status)
		}
		a.Status = *p.Status
	}
	set := func(dst, src *string) {
		if src != nil {
			*dst = *src
		}
	}
	set(&a.ProviderRequestID, p.ProviderRequestID)
	set(&a.UnsignedTxRef, p.UnsignedTxRef)
	set(&a.RecentBlockhash, p.RecentBlockhash)
	set(&a.SimulationRef, p.SimulationRef)
	set(&a.SigningDecisionID, p.SigningDecisionID)
	set(&a.SubmitResponseRef, p.SubmitResponseRef)
	set(&a.Error, p.Error)
	if p.UnsignedTxHash != nil {
		a.UnsignedTxHash = p.UnsignedTxHash
	}
	if p.LastValidBlockHeight != nil {
		a.LastValidBlockHeight = p.LastValidBlockHeight
	}
	if p.SimulationOK != nil {
		a.SimulationOK = p.SimulationOK
	}
	if p.InspectionResult != nil {
		a.InspectionResult = p.InspectionResult
	}
	if p.Finality != nil {
		a.Finality = *p.Finality
	}
	if p.SubmittedAt != nil {
		a.SubmittedAt = p.SubmittedAt
	}
	if p.ObservedAt != nil {
		a.ObservedAt = p.ObservedAt
	}
	if p.ConfirmedAt != nil {
		a.ConfirmedAt = p.ConfirmedAt
	}
	if p.FinalizedAt != nil {
		a.FinalizedAt = p.FinalizedAt
	}
	a.UpdatedAt = m.clk.Now()
	return *a, nil
}

// SetSignature implements AttemptStore.
func (m *MemAttempts) SetSignature(_ context.Context, _ pgx.Tx, attemptID execution.AttemptID, sig string, hash []byte) (execution.Attempt, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.attempts[attemptID]
	if !ok {
		return execution.Attempt{}, errs.New(errs.CodeNotFound, "attempt not found")
	}
	if a.TxSignature != "" {
		if a.TxSignature == sig {
			return *a, nil
		}
		return execution.Attempt{}, errs.New(errs.CodeConflict, "attempt already signed")
	}
	for _, x := range m.attempts {
		if x.TxSignature == sig {
			return execution.Attempt{}, errs.New(errs.CodeConflict, "signature belongs to another attempt")
		}
	}
	a.TxSignature, a.SignedTxHash = sig, hash
	return *a, nil
}

// ListForOrder implements AttemptStore.
func (m *MemAttempts) ListForOrder(_ context.Context, _ db.Querier, orderID execution.OrderID) ([]execution.Attempt, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []execution.Attempt
	for _, a := range m.attempts {
		if a.OrderID == orderID {
			out = append(out, *a)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AttemptNo < out[j].AttemptNo })
	return out, nil
}

// All returns every attempt by creation.
func (m *MemAttempts) All() []execution.Attempt {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]execution.Attempt, 0, len(m.attempts))
	for _, a := range m.attempts {
		out = append(out, *a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// --- quotes, intents ------------------------------------------------------------

// MemQuotes is an in-memory settlement.QuoteStore.
type MemQuotes struct {
	mu    sync.Mutex
	Saved []execution.QuoteSnapshot
}

// Save implements QuoteStore.
func (m *MemQuotes) Save(_ context.Context, _ pgx.Tx, q execution.QuoteSnapshot) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if q.ID == "" {
		q.ID = id.New[id.Any]().String()
	}
	m.Saved = append(m.Saved, q)
	return q.ID, nil
}

// MemIntents is an in-memory settlement.IntentReader.
type MemIntents struct {
	mu      sync.Mutex
	intents map[string]settlement.IntentRef
}

// NewMemIntents returns an empty reader.
func NewMemIntents() *MemIntents { return &MemIntents{intents: map[string]settlement.IntentRef{}} }

// Put registers an intent.
func (m *MemIntents) Put(ref settlement.IntentRef) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.intents[ref.ID] = ref
}

// Intent implements IntentReader.
func (m *MemIntents) Intent(_ context.Context, _ db.Querier, intentID string) (settlement.IntentRef, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ref, ok := m.intents[intentID]
	if !ok {
		return settlement.IntentRef{}, errs.New(errs.CodeNotFound, "intent not found")
	}
	return ref, nil
}

// --- capital -----------------------------------------------------------------------

// MemCapital is an in-memory settlement.CapitalService with the capital
// package's lifecycle rules (ACTIVE → CONSUMED | RELEASED, Consume only
// from ACTIVE, idempotent Reserve).
type MemCapital struct {
	mu       sync.Mutex
	clk      clock.Clock
	res      map[capital.ReservationID]*capital.Reservation
	byKey    map[string]capital.ReservationID
	Reserves int
	Consumes int
	Releases int
	Locks    int
	// FailReserve makes Reserve fail with INSUFFICIENT_BUYING_POWER.
	FailReserve bool
}

// NewMemCapital returns an empty service.
func NewMemCapital(clk clock.Clock) *MemCapital {
	return &MemCapital{clk: clk, res: map[capital.ReservationID]*capital.Reservation{}, byKey: map[string]capital.ReservationID{}}
}

// Reserve implements capital.Reserver.
func (m *MemCapital) Reserve(_ context.Context, _ pgx.Tx, r capital.ReserveRequest) (capital.Reservation, error) {
	if err := r.Validate(); err != nil {
		return capital.Reservation{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if rid, ok := m.byKey[r.IdempotencyKey]; ok {
		return *m.res[rid], nil
	}
	if m.FailReserve {
		return capital.Reservation{}, errs.New(errs.CodeInsufficientBuyingPower, "fake capital: insufficient")
	}
	acct, _ := accounts.ParseAccountID(r.AccountID)
	now := m.clk.Now()
	res := &capital.Reservation{
		ID: capital.NewReservationID(), AccountID: acct, AssetID: r.AssetID, EnvelopeID: r.EnvelopeID, IntentID: r.IntentID,
		ActorType: r.ActorType, ActorID: r.ActorID, Quantity: r.Quantity, USD: money.USDFromMinor(r.USDMinor), Status: capital.ReservationActive,
		Reason: r.Reason, IdempotencyKey: r.IdempotencyKey, CreatedAt: now, ExpiresAt: now.Add(r.TTL),
	}
	m.res[res.ID] = res
	m.byKey[r.IdempotencyKey] = res.ID
	m.Reserves++
	return *res, nil
}

// Consume implements capital.Reserver.
func (m *MemCapital) Consume(_ context.Context, _ pgx.Tx, rid capital.ReservationID, qty money.Quantity, usdMinor int64, orderID string) (capital.Reservation, error) {
	return m.consume(rid, qty, usdMinor, orderID, false)
}

// ConsumeFinal implements CapitalService.
func (m *MemCapital) ConsumeFinal(_ context.Context, _ pgx.Tx, rid capital.ReservationID, qty money.Quantity, usdMinor int64, orderID string) (capital.Reservation, error) {
	return m.consume(rid, qty, usdMinor, orderID, true)
}

func (m *MemCapital) consume(rid capital.ReservationID, qty money.Quantity, usdMinor int64, orderID string, final bool) (capital.Reservation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.res[rid]
	if !ok {
		return capital.Reservation{}, errs.New(errs.CodeNotFound, "reservation not found")
	}
	if r.Status != capital.ReservationActive {
		return capital.Reservation{}, errs.Newf(errs.CodeInvalidStateTransition, "reservation is %s", r.Status).WithField("from", string(r.Status))
	}
	if r.LockedByOrderID != "" && orderID != "" && orderID != r.LockedByOrderID {
		return capital.Reservation{}, errs.New(errs.CodeConflict, "reservation is locked by another order")
	}
	consumed := r.ConsumedQuantity.Add(qty)
	if consumed.Cmp(r.Quantity) > 0 {
		return capital.Reservation{}, errs.New(errs.CodeValidationFailed, "consumption exceeds reserved quantity")
	}
	usd, err := r.ConsumedUSD.Add(money.USDFromMinor(usdMinor))
	if err != nil {
		return capital.Reservation{}, err
	}
	if usd.Cmp(r.USD) > 0 {
		return capital.Reservation{}, errs.New(errs.CodeValidationFailed, "consumption exceeds reserved usd")
	}
	if (final || consumed.Equal(r.Quantity)) && consumed.IsZero() {
		return capital.Reservation{}, errs.New(errs.CodeValidationFailed, "nothing was consumed; release instead")
	}
	r.ConsumedQuantity, r.ConsumedUSD = consumed, usd
	m.Consumes++
	if final || consumed.Equal(r.Quantity) {
		r.Status = capital.ReservationConsumed
		t := m.clk.Now()
		r.ConsumedAt = &t
	}
	return *r, nil
}

// Release implements capital.Reserver.
func (m *MemCapital) Release(_ context.Context, _ pgx.Tx, rid capital.ReservationID, reason string) (capital.Reservation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.res[rid]
	if !ok {
		return capital.Reservation{}, errs.New(errs.CodeNotFound, "reservation not found")
	}
	if r.Status != capital.ReservationActive {
		return capital.Reservation{}, errs.Newf(errs.CodeInvalidStateTransition, "reservation is %s", r.Status)
	}
	r.Status, r.ReleaseReason = capital.ReservationReleased, reason
	t := m.clk.Now()
	r.ReleasedAt = &t
	m.Releases++
	return *r, nil
}

// LockForOrder implements capital.Reserver.
func (m *MemCapital) LockForOrder(_ context.Context, _ pgx.Tx, rid capital.ReservationID, orderID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.res[rid]
	if !ok {
		return errs.New(errs.CodeNotFound, "reservation not found")
	}
	if r.Status != capital.ReservationActive {
		return errs.New(errs.CodeInvalidStateTransition, "only ACTIVE reservations can be locked")
	}
	if r.LockedByOrderID == orderID {
		return nil
	}
	if r.LockedByOrderID != "" {
		return errs.New(errs.CodeConflict, "reservation is locked by another order")
	}
	r.LockedByOrderID = orderID
	m.Locks++
	return nil
}

// ExpireDue implements capital.Reserver (no-op).
func (m *MemCapital) ExpireDue(context.Context, pgx.Tx, time.Time, int) ([]capital.ReservationID, error) {
	return nil, nil
}

// Get implements CapitalService.
func (m *MemCapital) Get(_ context.Context, _ db.Querier, rid capital.ReservationID) (capital.Reservation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.res[rid]
	if !ok {
		return capital.Reservation{}, errs.New(errs.CodeNotFound, "reservation not found")
	}
	return *r, nil
}

// All returns every reservation.
func (m *MemCapital) All() []capital.Reservation {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]capital.Reservation, 0, len(m.res))
	for _, r := range m.res {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// --- ledger ----------------------------------------------------------------------------

// MemLedger is an in-memory ledger.Poster, idempotent on the posting key.
type MemLedger struct {
	mu     sync.Mutex
	byKey  map[string]ledger.TransactionID
	Posted []ledger.Posting
}

// NewMemLedger returns an empty poster.
func NewMemLedger() *MemLedger { return &MemLedger{byKey: map[string]ledger.TransactionID{}} }

// Post implements ledger.Poster.
func (m *MemLedger) Post(_ context.Context, _ pgx.Tx, p ledger.Posting) (ledger.PostResult, error) {
	if err := p.Validate(); err != nil {
		return ledger.PostResult{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if txID, ok := m.byKey[p.IdempotencyKey]; ok {
		return ledger.PostResult{TransactionID: txID, Existing: true}, nil
	}
	txID := ledger.NewTransactionID()
	m.byKey[p.IdempotencyKey] = txID
	m.Posted = append(m.Posted, p)
	return ledger.PostResult{TransactionID: txID}, nil
}

// Postings returns the number of distinct journal transactions.
func (m *MemLedger) Postings() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.Posted)
}

// --- positions -------------------------------------------------------------------------

// MemPositions is an in-memory positions.LotEngine (FIFO).
type MemPositions struct {
	mu           sync.Mutex
	lots         []*positions.Lot
	dispositions []positions.LotDisposition
	Acquires     int
	Disposes     int
}

// FillApplications counts position updates caused by fills: lots acquired
// from a fill plus disposals referencing a fill (network-fee disposals are a
// consequence of the same fill and are not counted).
func (m *MemPositions) FillApplications() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, l := range m.lots {
		if l.AcquisitionRef.Type == "fill" {
			n++
		}
	}
	seen := map[string]bool{}
	for _, d := range m.dispositions {
		if d.DispositionRef.Type == "fill" && !seen[d.DispositionRef.ID] {
			seen[d.DispositionRef.ID] = true
			n++
		}
	}
	return n
}

// Acquire implements LotEngine.
func (m *MemPositions) Acquire(_ context.Context, _ pgx.Tx, in positions.AcquireLot) (positions.Lot, error) {
	if err := in.Validate(); err != nil {
		return positions.Lot{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	basis, err := in.Cost.Add(in.Fees)
	if err != nil {
		return positions.Lot{}, err
	}
	lot := &positions.Lot{
		ID: positions.NewLotID(), AccountID: in.AccountID, AssetID: in.AssetID, QuantityOriginal: in.Quantity, QuantityOpen: in.Quantity,
		AcquiredAt: in.AcquiredAt, CostBasis: basis, Fees: in.Fees, BasisSource: in.BasisSource, ValuationSource: in.ValuationSource,
		AcquisitionRef: in.AcquisitionRef, Venue: in.Venue, WalletID: in.WalletID, JournalTxID: in.JournalTxID, Status: positions.LotOpen,
	}
	m.lots = append(m.lots, lot)
	m.Acquires++
	return *lot, nil
}

// Dispose implements LotEngine (FIFO, proportional basis without rounding
// subtleties: sufficient for executor tests).
func (m *MemPositions) Dispose(_ context.Context, _ pgx.Tx, d positions.Disposal) ([]positions.LotDisposition, positions.RealizedPnL, error) {
	if err := d.Validate(); err != nil {
		return nil, positions.RealizedPnL{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	open := money.Quantity{}
	for _, l := range m.lots {
		if l.AccountID == d.AccountID && l.AssetID == d.AssetID && l.Status == positions.LotOpen {
			open = open.Add(l.QuantityOpen)
		}
	}
	if open.Cmp(d.Quantity) < 0 {
		return nil, positions.RealizedPnL{}, errs.New(errs.CodeValidationFailed, "insufficient open quantity").WithField("open", open.String())
	}
	remaining := d.Quantity
	var out []positions.LotDisposition
	for _, l := range m.lots {
		if !remaining.IsPositive() {
			break
		}
		if l.AccountID != d.AccountID || l.AssetID != d.AssetID || l.Status != positions.LotOpen {
			continue
		}
		take := l.QuantityOpen.Min(remaining)
		l.QuantityOpen = l.QuantityOpen.Sub(take)
		if l.QuantityOpen.IsZero() {
			l.Status = positions.LotClosed
		}
		out = append(out, positions.LotDisposition{ID: positions.NewDispositionID(), LotID: l.ID, AccountID: d.AccountID, AssetID: d.AssetID, Quantity: take, DisposedAt: d.DisposedAt, Proceeds: d.Proceeds, Fees: d.Fees, DispositionRef: d.DispositionRef, JournalTxID: d.JournalTxID})
		remaining = remaining.Sub(take)
	}
	m.dispositions = append(m.dispositions, out...)
	m.Disposes++
	return out, positions.RealizedPnL{Quantity: d.Quantity, Proceeds: d.Proceeds, Fees: d.Fees, Dispositions: len(out)}, nil
}

// Holdings implements LotEngine.
func (m *MemPositions) Holdings(_ context.Context, _ db.Querier, accountID accounts.AccountID) ([]positions.Holding, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	byAsset := map[assets.AssetID]*positions.Holding{}
	for _, l := range m.lots {
		if l.AccountID != accountID || l.Status != positions.LotOpen {
			continue
		}
		h := byAsset[l.AssetID]
		if h == nil {
			h = &positions.Holding{AssetID: l.AssetID}
			byAsset[l.AssetID] = h
		}
		h.Quantity = h.Quantity.Add(l.QuantityOpen)
		h.LotCount++
	}
	out := make([]positions.Holding, 0, len(byAsset))
	for _, h := range byAsset {
		out = append(out, *h)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AssetID.String() < out[j].AssetID.String() })
	return out, nil
}

// VerifyAgainstLedger implements LotEngine (nothing to compare in memory).
func (m *MemPositions) VerifyAgainstLedger(context.Context, db.Querier, accounts.AccountID) ([]positions.Drift, error) {
	return nil, nil
}

// OpenQuantity returns the open quantity of an asset.
func (m *MemPositions) OpenQuantity(asset assets.AssetID) money.Quantity {
	m.mu.Lock()
	defer m.mu.Unlock()
	q := money.Quantity{}
	for _, l := range m.lots {
		if l.AssetID == asset && l.Status == positions.LotOpen {
			q = q.Add(l.QuantityOpen)
		}
	}
	return q
}

// --- kill switches, risk ------------------------------------------------------------

// KillSwitches is a settlement.KillSwitchChecker over a list of active
// switches, evaluated with the real killswitch.Blocking matrix.
type KillSwitches struct {
	mu     sync.Mutex
	Active []killswitch.Switch
	Policy killswitch.Policy
	Checks []killswitch.Action
}

// Activate adds an active switch.
func (k *KillSwitches) Activate(kind killswitch.Kind, scope string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.Active = append(k.Active, killswitch.Switch{ID: killswitch.NewSwitchID(), Kind: kind, ScopeID: scope, Active: true, Severity: kind.Severity()})
}

// Check implements KillSwitchChecker.
func (k *KillSwitches) Check(_ context.Context, _ db.Querier, a killswitch.Action) error {
	if err := a.Validate(); err != nil {
		return err
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	k.Checks = append(k.Checks, a)
	if sw, blocked := killswitch.Blocking(k.Active, a, k.Policy); blocked {
		return killswitch.BlockedError(sw, a)
	}
	return nil
}

// Snapshot implements KillSwitchChecker.
func (k *KillSwitches) Snapshot(context.Context, db.Querier) (killswitch.Snapshot, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return killswitch.Snapshot{Switches: append([]killswitch.Switch(nil), k.Active...)}, nil
}

// RiskFinal is a settlement.FinalRiskChecker returning a scripted verdict.
type RiskFinal struct {
	mu          sync.Mutex
	Verdict     risk.Verdict
	ReasonCodes []string
	Constraints risk.ResultingConstraints
	Calls       int
	Requests    []settlement.FinalRiskRequest
}

// CheckFinal implements FinalRiskChecker.
func (r *RiskFinal) CheckFinal(_ context.Context, _ pgx.Tx, req settlement.FinalRiskRequest) (risk.Decision, string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Calls++
	r.Requests = append(r.Requests, req)
	verdict := r.Verdict
	if verdict == "" {
		verdict = risk.Allow
	}
	d := risk.Decision{
		Verdict: verdict, Stage: risk.StageFinal, ActionClass: risk.ActionClass(req.Constraints.ActionClass), EffectiveNotionalUSD: req.Constraints.NotionalUSD,
		PolicyVersion: "risk-v1", PolicyHash: "risk-hash", EvaluatorVersion: risk.EvaluatorVersion, ReasonCodes: append([]string{}, r.ReasonCodes...),
		MatchedKillSwitches: []risk.KillSwitch{}, Constraints: r.Constraints, EvaluatedAt: req.Now,
	}
	d.Hash = d.ComputeHash()
	return d, id.New[id.Any]().String(), nil
}

// --- audit ----------------------------------------------------------------------------

// MemAudit is an in-memory audit.Writer that validates events exactly like
// the PostgreSQL writer and keeps a real hash chain per stream.
type MemAudit struct {
	mu     sync.Mutex
	chains map[string][]audit.Appended
	events map[string][]audit.Event
}

// NewMemAudit returns an empty writer.
func NewMemAudit() *MemAudit {
	return &MemAudit{chains: map[string][]audit.Appended{}, events: map[string][]audit.Event{}}
}

// Append implements audit.Writer.
func (m *MemAudit) Append(_ context.Context, _ pgx.Tx, e audit.Event) (audit.Appended, error) {
	if err := e.Validate(); err != nil {
		return audit.Appended{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	chain := m.chains[e.Stream]
	seq := int64(len(chain) + 1)
	var prev []byte
	if len(chain) > 0 {
		prev = chain[len(chain)-1].ContentHash
	}
	hash, err := audit.HashEvent(e, seq, prev, "test")
	if err != nil {
		return audit.Appended{}, err
	}
	ap := audit.Appended{ID: audit.NewEventID(), Stream: e.Stream, StreamSeq: seq, ContentHash: hash, PrevHash: prev}
	m.chains[e.Stream] = append(chain, ap)
	m.events[e.Stream] = append(m.events[e.Stream], e)
	return ap, nil
}

// Events returns the events recorded on a stream, in order.
func (m *MemAudit) Events(stream string) []audit.Event {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]audit.Event(nil), m.events[stream]...)
}

// Actions returns the action names recorded on a stream, in order.
func (m *MemAudit) Actions(stream string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.events[stream]))
	for _, e := range m.events[stream] {
		out = append(out, e.Action)
	}
	return out
}
