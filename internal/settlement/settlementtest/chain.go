// Package settlementtest provides scripted fakes for the settlement executor:
// an ExecutionAdapter with per-call outcomes and effect counters, a shared
// fake chain, a chain observer, a recoverer, a signer, an inspector, an
// archive and in-memory stores for every executor dependency. It is never
// wired into production (the "<pkg>/<pkg>test" rule is enforced by
// scripts/lintfin).
package settlementtest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/execution"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/settlement"
)

// TxRecord is one landed transaction on the fake chain.
type TxRecord struct {
	Signature string
	Slot      uint64
	Finality  execution.FinalityLevel
	Failed    bool
	Error     string
	Fills     []execution.ExternalExecutionEvent
	LandedAt  time.Time
}

// Chain is the shared external truth the adapter, the observer and the
// recoverer all read. Tests script disagreement through the observer.
type Chain struct {
	mu       sync.Mutex
	clk      clock.Clock
	height   uint64
	txs      map[string]*TxRecord
	balances map[string]map[string]money.Quantity
	landed   int
	// LandFinality is the finality a landed transaction reports; default
	// CONFIRMED.
	LandFinality execution.FinalityLevel
}

// NewChain returns an empty chain at height 1000.
func NewChain(clk clock.Clock) *Chain {
	return &Chain{clk: clk, height: 1000, txs: map[string]*TxRecord{}, balances: map[string]map[string]money.Quantity{}, LandFinality: execution.FinalityConfirmed}
}

// SetBalance sets a wallet's balance in a mint.
func (c *Chain) SetBalance(wallet, mint string, q money.Quantity) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.balances[wallet] == nil {
		c.balances[wallet] = map[string]money.Quantity{}
	}
	c.balances[wallet][mint] = q
}

// Balance returns a wallet's balance in a mint.
func (c *Chain) Balance(wallet, mint string) (money.Quantity, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	q, ok := c.balances[wallet][mint]
	return q, ok
}

// Land records a transaction as executed with the given fills. Landing the
// same signature twice is a programming error in the fake: the real chain
// cannot execute one signature twice, and the count is the economic-effect
// witness of the executor tests.
func (c *Chain) Land(sig string, fills []execution.ExternalExecutionEvent) *TxRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, dup := c.txs[sig]; dup {
		panic("settlementtest: signature " + sig + " landed twice")
	}
	c.height++
	rec := &TxRecord{Signature: sig, Slot: c.height, Finality: c.LandFinality, Fills: fills, LandedAt: c.clk.Now()}
	c.txs[sig] = rec
	c.landed++
	return rec
}

// Fail records a transaction as executed and failed.
func (c *Chain) Fail(sig, msg string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.height++
	c.txs[sig] = &TxRecord{Signature: sig, Slot: c.height, Finality: execution.FinalityConfirmed, Failed: true, Error: msg, LandedAt: c.clk.Now()}
}

// Lookup returns a landed transaction.
func (c *Chain) Lookup(sig string) (TxRecord, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	rec, ok := c.txs[sig]
	if !ok {
		return TxRecord{}, false
	}
	cp := *rec
	cp.Fills = append([]execution.ExternalExecutionEvent(nil), rec.Fills...)
	return cp, true
}

// SetFinality upgrades (or downgrades) a landed transaction's finality.
func (c *Chain) SetFinality(sig string, l execution.FinalityLevel) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if rec, ok := c.txs[sig]; ok {
		rec.Finality = l
	}
}

// Height returns the chain height.
func (c *Chain) Height() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.height
}

// SetHeight sets the chain height.
func (c *Chain) SetHeight(h uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.height = h
}

// Landed returns how many transactions executed: the number of economic
// effects.
func (c *Chain) Landed() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.landed
}

// Signatures returns every landed signature, sorted.
func (c *Chain) Signatures() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.txs))
	for s := range c.txs {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// SubmitKind scripts one Submit call.
type SubmitKind string

// Submit outcomes.
const (
	// SubmitOK: accepted and landed.
	SubmitOK SubmitKind = "OK"
	// SubmitTimeoutLanded: the transport timed out but the transaction
	// landed (the PART 48 case).
	SubmitTimeoutLanded SubmitKind = "TIMEOUT_LANDED"
	// SubmitTimeoutLost: the transport timed out and nothing landed.
	SubmitTimeoutLost SubmitKind = "TIMEOUT_LOST"
	// SubmitReject: definitive rejection, nothing landed.
	SubmitReject SubmitKind = "REJECT"
	// SubmitRejectLanded: the provider says rejected but the transaction is
	// on chain.
	SubmitRejectLanded SubmitKind = "REJECT_LANDED"
	// SubmitOKFailedOnChain: accepted, landed, but the transaction failed.
	SubmitOKFailedOnChain SubmitKind = "OK_FAILED_ON_CHAIN"
)

// builtTx is what the fake Build encodes into the transaction bytes so the
// fake Submit can derive the fill from the signed bytes alone.
type builtTx struct {
	QuoteID        string         `json:"quote_id"`
	AttemptNo      int32          `json:"attempt_no"`
	Side           execution.Side `json:"side"`
	InputAsset     assets.AssetID `json:"input_asset"`
	OutputAsset    assets.AssetID `json:"output_asset"`
	Input          money.Quantity `json:"input"`
	ExpectedOutput money.Quantity `json:"expected_output"`
	MinOutput      money.Quantity `json:"min_output"`
	Wallet         string         `json:"wallet"`
	Blockhash      string         `json:"blockhash"`
}

// Adapter is a scripted execution.ExecutionAdapter. Unscripted calls take
// the happy path; every call is counted.
type Adapter struct {
	mu    sync.Mutex
	name  string
	chain *Chain
	clk   clock.Clock

	// Rates give expected output = input × Num / Den per side.
	BuyRateNum, BuyRateDen   money.Quantity
	SellRateNum, SellRateDen money.Quantity
	QuoteExpiry              time.Duration
	QuoteSlippageBPS         money.BPS
	QuotePriceImpactBPS      money.BPS
	VenueFeeBPS              money.BPS
	NetworkFee               money.Quantity
	NetworkFeeAsset          assets.AssetID
	// FillOutputBPS scales the actual fill output relative to the expected
	// output (10_000 = exactly expected).
	FillOutputBPS money.BPS

	SubmitScript      []SubmitKind
	QuoteErr          error
	ValidateErr       error
	BuildErr          error
	StatusErr         error
	ReconcileErr      error
	CancelUnsupported bool

	Calls       map[execution.AdapterMethod]int
	Submissions int
	Cancels     int
	builds      int
	requests    int
}

// NewAdapter returns an adapter over chain named "jupiter".
func NewAdapter(chain *Chain, clk clock.Clock) *Adapter {
	return &Adapter{
		name: "jupiter", chain: chain, clk: clk,
		BuyRateNum: money.QuantityFromInt64(20), BuyRateDen: money.QuantityFromInt64(3),
		SellRateNum: money.QuantityFromInt64(3), SellRateDen: money.QuantityFromInt64(20),
		QuoteExpiry: 30 * time.Second, QuoteSlippageBPS: 50, QuotePriceImpactBPS: 5,
		NetworkFee: money.QuantityFromInt64(5000), FillOutputBPS: money.OneHundredPercent,
		Calls: map[execution.AdapterMethod]int{},
	}
}

func (a *Adapter) count(m execution.AdapterMethod) {
	a.mu.Lock()
	a.Calls[m]++
	a.mu.Unlock()
}

// Name implements ExecutionAdapter.
func (a *Adapter) Name() string { return a.name }

// Quote implements ExecutionAdapter.
func (a *Adapter) Quote(_ context.Context, req execution.QuoteRequest) (execution.QuoteSnapshot, error) {
	a.count(execution.MethodQuote)
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.QuoteErr != nil {
		return execution.QuoteSnapshot{}, a.QuoteErr
	}
	num, den := a.BuyRateNum, a.BuyRateDen
	if req.Side == execution.SideSell {
		num, den = a.SellRateNum, a.SellRateDen
	}
	expected, err := req.InputQuantity.MulDiv(num, den, money.RoundDown)
	if err != nil {
		return execution.QuoteSnapshot{}, err
	}
	slippage := a.QuoteSlippageBPS
	if req.MaxSlippageBPS > 0 && req.MaxSlippageBPS < slippage {
		slippage = req.MaxSlippageBPS
	}
	minOut := expected.MulBPS(money.OneHundredPercent-slippage, money.RoundDown)
	if minOut.Cmp(req.MinOutput) < 0 && req.MinOutput.Cmp(expected) <= 0 {
		minOut = req.MinOutput
	}
	quoteLeg := req.InputQuantity
	if req.Side == execution.SideSell {
		quoteLeg = expected
	}
	a.requests++
	now := a.clk.Now()
	route := json.RawMessage(fmt.Sprintf(`[{"venue":%q,"pool":"fake"}]`, a.name))
	sum := sha256.Sum256(route)
	return execution.QuoteSnapshot{
		IntentID: req.IntentID, Provider: a.name, ProviderRequestID: fmt.Sprintf("qreq-%d", a.requests), InstrumentID: req.InstrumentID,
		VenueListingID: req.VenueListingID, Side: req.Side, InputAsset: req.InputAsset, InputQuantity: req.InputQuantity, OutputAsset: req.OutputAsset,
		ExpectedOutput: expected, MinimumOutput: minOut,
		EffectivePrice: money.Price{Mantissa: money.QuantityFromInt64(150), Scale: 0, QuoteAsset: "USDC", Source: a.name, At: now},
		PriceImpactBPS: a.QuotePriceImpactBPS, SlippageBPS: slippage,
		EstNetworkCost: a.NetworkFee, EstNetworkAsset: a.NetworkFeeAsset,
		EstVenueFee: quoteLeg.MulBPS(a.VenueFeeBPS, money.RoundUp), EstVenueFeeAsset: quoteAsset(req),
		ReceivedAt: now, ExpiresAt: now.Add(a.QuoteExpiry), RouteHash: sum[:], RouteSummary: route,
	}, nil
}

func quoteAsset(req execution.QuoteRequest) assets.AssetID {
	if req.Side == execution.SideBuy {
		return req.InputAsset
	}
	return req.OutputAsset
}

// ValidateQuote implements ExecutionAdapter.
func (a *Adapter) ValidateQuote(context.Context, execution.QuoteSnapshot) error {
	a.count(execution.MethodValidateQuote)
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.ValidateErr
}

// Build implements ExecutionAdapter. The bytes are the JSON of builtTx so
// Submit can derive the fill from the signed bytes.
func (a *Adapter) Build(_ context.Context, req execution.BuildRequest) (execution.UnsignedAction, error) {
	a.count(execution.MethodBuild)
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.BuildErr != nil {
		return execution.UnsignedAction{}, a.BuildErr
	}
	a.builds++
	bh := fmt.Sprintf("blockhash-%d", a.builds)
	body, err := json.Marshal(builtTx{
		QuoteID: req.Quote.ID, AttemptNo: req.AttemptNo, Side: req.Quote.Side, InputAsset: req.Quote.InputAsset, OutputAsset: req.Quote.OutputAsset,
		Input: req.Quote.InputQuantity, ExpectedOutput: req.Quote.ExpectedOutput, MinOutput: req.MinOutput, Wallet: req.WalletAddress, Blockhash: bh,
	})
	if err != nil {
		return execution.UnsignedAction{}, err
	}
	sum := sha256.Sum256(body)
	return execution.UnsignedAction{
		Chain: "solana-devnet", Bytes: body, Hash: sum[:], RecentBlockhash: bh, LastValidBlockHeight: a.chain.Height() + 150,
		ExpiresAt: a.clk.Now().Add(90 * time.Second), ProviderRequestID: fmt.Sprintf("breq-%d", a.builds),
	}, nil
}

// SignatureFor is the signature the fake Signer produces for unsigned bytes.
func SignatureFor(unsigned []byte) string {
	sum := sha256.Sum256(unsigned)
	return "sig-" + hex.EncodeToString(sum[:])[:32]
}

// UnsignedOf strips the fake signature suffix from signed bytes.
func UnsignedOf(signed []byte) []byte {
	s := string(signed)
	if i := strings.LastIndex(s, "|signed"); i >= 0 {
		return []byte(s[:i])
	}
	return signed
}

// Submit implements ExecutionAdapter with the scripted outcome. Landing the
// transaction is the economic effect; Submissions counts it.
func (a *Adapter) Submit(_ context.Context, req execution.SignedSubmission) (execution.SubmissionResult, error) {
	a.count(execution.MethodSubmit)
	a.mu.Lock()
	kind := SubmitOK
	if len(a.SubmitScript) > 0 {
		kind, a.SubmitScript = a.SubmitScript[0], a.SubmitScript[1:]
	}
	a.mu.Unlock()
	var tx builtTx
	if err := json.Unmarshal(UnsignedOf(req.SignedTx), &tx); err != nil {
		return execution.SubmissionResult{}, errs.Wrap(err, errs.CodeValidationFailed, "fake adapter: malformed transaction")
	}
	sig := req.TxSignature
	if sig == "" {
		sig = SignatureFor(UnsignedOf(req.SignedTx))
	}
	land := func() {
		a.mu.Lock()
		a.Submissions++
		a.mu.Unlock()
		a.chain.Land(sig, []execution.ExternalExecutionEvent{a.fillFor(tx, sig)})
	}
	now := a.clk.Now()
	switch kind {
	case SubmitOK:
		land()
		return execution.SubmissionResult{ExternalRef: execution.ExternalReference{Venue: a.name, TxSignature: sig, ProviderRequestID: "sreq-" + sig[4:12], WalletAddress: tx.Wallet}, AcceptedAt: now}, nil
	case SubmitOKFailedOnChain:
		a.mu.Lock()
		a.Submissions++
		a.mu.Unlock()
		a.chain.Fail(sig, "custom program error: slippage exceeded")
		return execution.SubmissionResult{ExternalRef: execution.ExternalReference{Venue: a.name, TxSignature: sig, WalletAddress: tx.Wallet}, AcceptedAt: now}, nil
	case SubmitTimeoutLanded:
		land()
		return execution.SubmissionResult{}, errs.New(errs.CodeSubmissionStateUnknown, "fake adapter: transport timeout")
	case SubmitTimeoutLost:
		return execution.SubmissionResult{}, errs.New(errs.CodeSubmissionStateUnknown, "fake adapter: transport timeout")
	case SubmitReject:
		return execution.SubmissionResult{}, errs.New(errs.CodeValidationFailed, "fake adapter: transaction rejected: blockhash not found")
	case SubmitRejectLanded:
		land()
		return execution.SubmissionResult{}, errs.New(errs.CodeValidationFailed, "fake adapter: transaction rejected (but it landed)")
	}
	return execution.SubmissionResult{}, errs.New(errs.CodeInternal, "fake adapter: unknown submit kind "+string(kind))
}

func (a *Adapter) fillFor(tx builtTx, sig string) execution.ExternalExecutionEvent {
	a.mu.Lock()
	outBPS := a.FillOutputBPS
	fee, feeAsset, venueFeeBPS := a.NetworkFee, a.NetworkFeeAsset, a.VenueFeeBPS
	a.mu.Unlock()
	out := tx.ExpectedOutput.MulBPS(outBPS, money.RoundDown)
	quoteLeg := tx.Input
	if tx.Side == execution.SideSell {
		quoteLeg = out
	}
	return execution.ExternalExecutionEvent{
		Venue: a.name, ExternalFillID: "fill-" + sig, TxSignature: sig, InputAsset: tx.InputAsset, OutputAsset: tx.OutputAsset,
		InputQty: tx.Input, OutputQty: out, NetworkFee: fee, NetworkFeeAsset: feeAsset, VenueFee: quoteLeg.MulBPS(venueFeeBPS, money.RoundUp),
		ObservedAt: a.clk.Now(), Finality: string(execution.FinalityConfirmed),
	}
}

// Status implements ExecutionAdapter from the chain.
func (a *Adapter) Status(_ context.Context, ref execution.ExternalReference) (execution.ExecutionStatus, error) {
	a.count(execution.MethodStatus)
	a.mu.Lock()
	err := a.StatusErr
	a.mu.Unlock()
	if err != nil {
		return execution.ExecutionStatus{}, err
	}
	rec, ok := a.chain.Lookup(ref.TxSignature)
	if !ok {
		return execution.ExecutionStatus{State: execution.ExternalNotFound}, nil
	}
	if rec.Failed {
		return execution.ExecutionStatus{State: execution.ExternalFailed, Slot: rec.Slot, Error: rec.Error}, nil
	}
	fills := make([]execution.ExternalExecutionEvent, len(rec.Fills))
	for i, f := range rec.Fills {
		f.Finality = string(rec.Finality)
		fills[i] = f
	}
	return execution.ExecutionStatus{State: stateOf(rec.Finality), Fills: fills, Slot: rec.Slot}, nil
}

func stateOf(l execution.FinalityLevel) execution.ExternalState {
	switch l {
	case execution.FinalityObserved:
		return execution.ExternalObserved
	case execution.FinalityConfirmed:
		return execution.ExternalConfirmed
	case execution.FinalityFinalized:
		return execution.ExternalFinalized
	}
	return execution.ExternalPending
}

// Cancel implements ExecutionAdapter.
func (a *Adapter) Cancel(context.Context, execution.ExternalReference) error {
	a.count(execution.MethodCancel)
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.CancelUnsupported {
		return errs.New(errs.CodeUnsupported, "fake adapter: atomic swaps cannot be cancelled")
	}
	a.Cancels++
	return nil
}

// Reconcile implements ExecutionAdapter from the chain.
func (a *Adapter) Reconcile(_ context.Context, scope execution.ReconcileScope) ([]execution.ExternalExecutionEvent, error) {
	a.count(execution.MethodReconcile)
	a.mu.Lock()
	err := a.ReconcileErr
	a.mu.Unlock()
	if err != nil {
		return nil, err
	}
	var out []execution.ExternalExecutionEvent
	for _, sig := range a.chain.Signatures() {
		if scope.TxSignature != "" && scope.TxSignature != sig {
			continue
		}
		rec, _ := a.chain.Lookup(sig)
		if rec.Failed {
			continue
		}
		for _, f := range rec.Fills {
			f.Finality = string(rec.Finality)
			out = append(out, f)
		}
	}
	return out, nil
}

// Observer is a settlement.ChainObserver over the fake chain.
type Observer struct {
	mu    sync.Mutex
	chain *Chain
	// Blind makes GetTransaction never find anything (disagreement).
	Blind bool
	Err   error
	Calls int
}

// NewObserver returns an observer over chain.
func NewObserver(chain *Chain) *Observer { return &Observer{chain: chain} }

// Name implements ChainObserver.
func (o *Observer) Name() string { return "fake-observer" }

// GetTransaction implements ChainObserver.
func (o *Observer) GetTransaction(_ context.Context, sig string) (settlement.TxObservation, error) {
	o.mu.Lock()
	o.Calls++
	blind, err := o.Blind, o.Err
	o.mu.Unlock()
	if err != nil {
		return settlement.TxObservation{}, err
	}
	if blind {
		return settlement.TxObservation{Signature: sig}, nil
	}
	rec, ok := o.chain.Lookup(sig)
	if !ok {
		return settlement.TxObservation{Signature: sig}, nil
	}
	return settlement.TxObservation{Signature: sig, Found: true, Slot: rec.Slot, Finality: rec.Finality, Failed: rec.Failed, Error: rec.Error, ObservedAt: rec.LandedAt}, nil
}

// GetBalances implements ChainObserver.
func (o *Observer) GetBalances(_ context.Context, wallet string, mints []string) ([]settlement.BalanceObservation, error) {
	o.mu.Lock()
	err := o.Err
	o.mu.Unlock()
	if err != nil {
		return nil, err
	}
	var out []settlement.BalanceObservation
	for _, m := range mints {
		if q, ok := o.chain.Balance(wallet, m); ok {
			out = append(out, settlement.BalanceObservation{Mint: m, Quantity: q, ObservedAt: o.chain.clk.Now()})
		}
	}
	return out, nil
}

// GetBlockHeight implements ChainObserver.
func (o *Observer) GetBlockHeight(context.Context) (uint64, error) { return o.chain.Height(), nil }

// Recoverer is a settlement.Recoverer over the fake chain: found → ADOPTED;
// absent past the last valid block height (+32) → PROVEN_ABSENT; otherwise
// UNRESOLVED. Force overrides the verdict.
type Recoverer struct {
	mu       sync.Mutex
	chain    *Chain
	Force    settlement.RecoveryOutcome
	Calls    int
	Requests []settlement.RecoveryRequest
}

// NewRecoverer returns a recoverer over chain.
func NewRecoverer(chain *Chain) *Recoverer { return &Recoverer{chain: chain} }

// Recover implements settlement.Recoverer.
func (r *Recoverer) Recover(_ context.Context, req settlement.RecoveryRequest) (settlement.RecoveryResult, error) {
	r.mu.Lock()
	r.Calls++
	r.Requests = append(r.Requests, req)
	force := r.Force
	r.mu.Unlock()
	ref := execution.ExternalReference{Venue: req.Provider, TxSignature: req.TxSignature, WalletAddress: req.WalletAddress}
	if force != "" {
		return settlement.RecoveryResult{Outcome: force, ExternalRef: ref, EvidenceRef: "mem://recovery/forced", Reason: "forced by test"}, nil
	}
	if rec, ok := r.chain.Lookup(req.TxSignature); ok {
		return settlement.RecoveryResult{
			Outcome: settlement.RecoveryAdopted, ExternalRef: ref, EvidenceRef: "mem://recovery/" + req.TxSignature,
			Status: execution.ExecutionStatus{State: stateOf(rec.Finality), Fills: rec.Fills, Slot: rec.Slot}, Reason: "signature found on chain",
		}, nil
	}
	if req.LastValidBlockHeight > 0 && r.chain.Height() > req.LastValidBlockHeight+32 {
		return settlement.RecoveryResult{Outcome: settlement.RecoveryProvenAbsent, ExternalRef: ref, EvidenceRef: "mem://recovery/absent", Reason: "block height passed last valid block height"}, nil
	}
	return settlement.RecoveryResult{Outcome: settlement.RecoveryUnresolved, ExternalRef: ref, EvidenceRef: "mem://recovery/unresolved", Reason: "signature not found yet"}, nil
}

// Signer is a fake settlement.Signer that counts calls. OmitDecisionID
// leaves SignResult.DecisionID empty for tests on a real database, where no
// signing_decisions row backs the fake's decision.
type Signer struct {
	mu             sync.Mutex
	Calls          int
	Reject         bool
	OmitDecisionID bool
	ReasonCodes    []string
	Requests       []settlement.SignRequest
}

// Sign implements settlement.Signer.
func (s *Signer) Sign(_ context.Context, req settlement.SignRequest) (settlement.SignResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Calls++
	s.Requests = append(s.Requests, req)
	if s.Reject {
		codes := s.ReasonCodes
		if len(codes) == 0 {
			codes = []string{"PROGRAM_ALLOWLIST"}
		}
		return settlement.SignResult{DecisionID: id.New[id.Any]().String(), Approved: false, ReasonCodes: codes}, nil
	}
	signed := append(append([]byte(nil), req.UnsignedTx...), []byte("|signed")...)
	decision := id.New[id.Any]().String()
	if s.OmitDecisionID {
		decision = ""
	}
	return settlement.SignResult{DecisionID: decision, Approved: true, SignedTx: signed, TxSignature: SignatureFor(req.UnsignedTx)}, nil
}

// CallCount returns the number of Sign calls.
func (s *Signer) CallCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Calls
}

// Inspector is a fake settlement.TransactionInspector.
type Inspector struct {
	mu       sync.Mutex
	Calls    int
	Reject   bool
	Reasons  []string
	Requests []settlement.InspectRequest
}

// Inspect implements settlement.TransactionInspector.
func (i *Inspector) Inspect(_ context.Context, req settlement.InspectRequest) (settlement.InspectResult, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.Calls++
	i.Requests = append(i.Requests, req)
	ok := true
	if i.Reject {
		reasons := i.Reasons
		if len(reasons) == 0 {
			reasons = []string{"NO_UNEXPECTED_DESTINATION"}
		}
		return settlement.InspectResult{Approved: false, ReasonCodes: reasons, Checks: json.RawMessage(`[{"check":"NO_UNEXPECTED_DESTINATION","ok":false}]`), InspectorVersion: "fake-inspector/1", SimulationOK: &ok}, nil
	}
	return settlement.InspectResult{Approved: true, Checks: json.RawMessage(`[{"check":"FEE_PAYER","ok":true}]`), InspectorVersion: "fake-inspector/1", SimulationOK: &ok, SimulationRef: "mem://sim"}, nil
}

// Archive is an in-memory execution.ArchiveWriter.
type Archive struct {
	mu      sync.Mutex
	Objects map[string][]byte
	Puts    int
}

// NewArchive returns an empty archive.
func NewArchive() *Archive { return &Archive{Objects: map[string][]byte{}} }

// Put implements ArchiveWriter.
func (a *Archive) Put(_ context.Context, key, _ string, body []byte) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.Objects[key] = append([]byte(nil), body...)
	a.Puts++
	return "mem://" + key, nil
}
