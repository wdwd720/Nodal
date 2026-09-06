package jupiter

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/gagliardetto/solana-go"

	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/provider"
)

// Fault is an injectable failure mode for the Fake.
type Fault string

// Faults. Each documents the live behavior it imitates.
const (
	FaultNone Fault = ""
	// FaultTimeout: Order/Build -> PROVIDER_UNAVAILABLE (timed_out); Execute
	// -> the submission is recorded as landed but SUBMISSION_STATE_UNKNOWN
	// is returned (provider succeeded, response lost).
	FaultTimeout Fault = "TIMEOUT"
	// FaultExpired: Order -> an order whose ExpiresAt and lastValidBlockHeight
	// are already in the past; Execute -> QUOTE_EXPIRED (code -1, missing
	// cached order, not submitted).
	FaultExpired Fault = "EXPIRED"
	// FaultSlippageExceeded: Execute -> status Failed with a code in the
	// documented aggregator failure range; nothing landed.
	FaultSlippageExceeded Fault = "SLIPPAGE_EXCEEDED"
	// FaultRateLimited: RATE_LIMITED with RetryAfter; nothing submitted.
	FaultRateLimited Fault = "RATE_LIMITED"
	// FaultServerError: Order/Build -> PROVIDER_UNAVAILABLE; Execute ->
	// SUBMISSION_STATE_UNKNOWN (documented 500 {signature, error}).
	FaultServerError Fault = "SERVER_ERROR"
	// FaultInvalidResponse: Order/Build -> VALIDATION_FAILED (float-formatted
	// amount on the wire); Execute -> SUBMISSION_STATE_UNKNOWN (undecodable).
	FaultInvalidResponse Fault = "INVALID_RESPONSE"
)

// FakeRate is the exact conversion out = in x Num / Den (RoundDown).
type FakeRate struct {
	Num int64
	Den int64
}

// FakeConfig configures a Fake. Zero values take the documented defaults
// noted on each field.
type FakeConfig struct {
	// Env must allow fake providers (LOCAL, TEST, DEV).
	Env config.Environment
	// Clock is required.
	Clock clock.Clock
	// Archive is optional; when nil RawRef is "fake:<hash prefix>".
	Archive RawArchive
	// ProgramID defaults to DefaultProgramID.
	ProgramID string
	// DefaultRate defaults to 1:1; Rates keys are "<inputMint>><outputMint>".
	DefaultRate FakeRate
	Rates       map[string]FakeRate
	// DefaultSlippageBPS applies when the request leaves slippage to the
	// provider (RTSE); default 50.
	DefaultSlippageBPS money.BPS
	// StartBlockHeight (default 250_000_000) and BlockHeightValidity
	// (default 150 blocks) drive lastValidBlockHeight.
	StartBlockHeight    uint64
	BlockHeightValidity uint64
	// OrderTTL is the assumed wall-clock validity (default 30s).
	OrderTTL time.Duration
	// Transaction parameters.
	ComputeUnitLimit              uint32 // default 200_000
	ComputeUnitPriceMicroLamports uint64 // default 1_000
	PrioritizationFeeLamports     int64  // default 5_000
	SignatureFeeLamports          int64  // default 5_000
	// Quote parameters.
	PriceImpactBPS money.BPS // default 5
	FeeBPS         money.BPS // default 10 (documented "other" tier)
	// RetryAfter for FaultRateLimited (default 1s).
	RetryAfter time.Duration
}

// FakeSubmission records one Execute the Fake accepted.
type FakeSubmission struct {
	RequestID string
	Signature string
	Status    ExecuteStatus
	Slot      uint64
	At        time.Time
	// ResponseLost is true when FaultTimeout hid a successful landing.
	ResponseLost bool
}

type fakeOrder struct {
	order     Order
	blockhash solana.Hash
}

// Fake is the deterministic in-process double. Orders and transactions are
// derived from the request and a sequence number only, so two runs with
// the same calls produce identical bytes. It is safe for concurrent use.
type Fake struct {
	cfg       FakeConfig
	programID solana.PublicKey

	mu          sync.Mutex
	faults      map[Operation]Fault
	seq         uint64
	blockHeight uint64
	orders      map[string]fakeOrder
	submissions []FakeSubmission
}

// Compile-time check.
var _ Service = (*Fake)(nil)

// NewFake constructs a Fake. It refuses environments that do not allow
// fake providers.
func NewFake(cfg FakeConfig) (*Fake, error) {
	if !cfg.Env.AllowsFakeProviders() {
		return nil, fmt.Errorf("jupiter: fake provider is not allowed in %q", string(cfg.Env))
	}
	if cfg.Clock == nil {
		return nil, errors.New("jupiter: fake requires a Clock")
	}
	if cfg.ProgramID == "" {
		cfg.ProgramID = DefaultProgramID
	}
	pid, err := solana.PublicKeyFromBase58(cfg.ProgramID)
	if err != nil {
		return nil, errors.New("jupiter: fake ProgramID is not a base58 public key")
	}
	if cfg.DefaultRate.Num <= 0 || cfg.DefaultRate.Den <= 0 {
		cfg.DefaultRate = FakeRate{Num: 1, Den: 1}
	}
	for k, r := range cfg.Rates {
		if r.Num <= 0 || r.Den <= 0 {
			return nil, fmt.Errorf("jupiter: fake rate %q must be positive", k)
		}
	}
	if cfg.DefaultSlippageBPS <= 0 {
		cfg.DefaultSlippageBPS = 50
	}
	if cfg.StartBlockHeight == 0 {
		cfg.StartBlockHeight = 250_000_000
	}
	if cfg.BlockHeightValidity == 0 {
		cfg.BlockHeightValidity = 150
	}
	if cfg.OrderTTL <= 0 {
		cfg.OrderTTL = DefaultAssumedOrderTTL
	}
	if cfg.ComputeUnitLimit == 0 {
		cfg.ComputeUnitLimit = 200_000
	}
	if cfg.ComputeUnitPriceMicroLamports == 0 {
		cfg.ComputeUnitPriceMicroLamports = 1_000
	}
	if cfg.PrioritizationFeeLamports == 0 {
		cfg.PrioritizationFeeLamports = 5_000
	}
	if cfg.SignatureFeeLamports == 0 {
		cfg.SignatureFeeLamports = 5_000
	}
	if cfg.PriceImpactBPS == 0 {
		cfg.PriceImpactBPS = 5
	}
	if cfg.FeeBPS == 0 {
		cfg.FeeBPS = 10
	}
	if cfg.RetryAfter <= 0 {
		cfg.RetryAfter = time.Second
	}
	return &Fake{
		cfg: cfg, programID: pid,
		faults: map[Operation]Fault{}, blockHeight: cfg.StartBlockHeight, orders: map[string]fakeOrder{},
	}, nil
}

// Name implements Service.
func (f *Fake) Name() string { return ProviderName }

// VerificationLabel implements Service.
func (f *Fake) VerificationLabel() provider.VerificationLabel { return provider.CodeComplete }

// ValidateQuote implements Service.
func (f *Fake) ValidateQuote(o Order, now time.Time, p ValidationPolicy) error {
	return ValidateQuote(o, now, p)
}

// Status implements Service: UNSUPPORTED, exactly like the live client.
func (f *Fake) Status(_ context.Context, signature string) (StatusResult, error) {
	return StatusResult{Signature: signature}, errs.New(errs.CodeUnsupported,
		"jupiter: Swap API V2 has no status endpoint; observe the signature through the chain observers").
		WithField(fieldOperation, string(OpStatus)).WithField(fieldSignature, signature)
}

// InjectFault makes the next calls of op fail with fault until cleared.
func (f *Fake) InjectFault(op Operation, fault Fault) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if fault == FaultNone {
		delete(f.faults, op)
		return
	}
	f.faults[op] = fault
}

// ClearFaults removes every injected fault.
func (f *Fake) ClearFaults() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.faults = map[Operation]Fault{}
}

// AdvanceBlockHeight moves the simulated chain forward by n blocks.
func (f *Fake) AdvanceBlockHeight(n uint64) uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.blockHeight += n
	return f.blockHeight
}

// BlockHeight returns the simulated current block height.
func (f *Fake) BlockHeight() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.blockHeight
}

// Submissions returns every accepted submission (copy).
func (f *Fake) Submissions() []FakeSubmission {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]FakeSubmission(nil), f.submissions...)
}

func (f *Fake) fault(op Operation) Fault {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.faults[op]
}

func (f *Fake) rate(in, out string) FakeRate {
	if r, ok := f.cfg.Rates[in+">"+out]; ok {
		return r
	}
	return f.cfg.DefaultRate
}

func (f *Fake) nextSeq() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	return f.seq
}

// archive stores synthetic evidence and returns the reference.
func (f *Fake) archive(ctx context.Context, op Operation, reqURL string, reqBody, respBody []byte, status int, at time.Time) (string, []byte, error) {
	hash := sha256Of(respBody)
	if f.cfg.Archive == nil {
		return "fake:" + hex.EncodeToString(hash[:8]), hash, nil
	}
	ref, err := f.cfg.Archive.Archive(ctx, Evidence{
		Provider: ProviderName, Operation: op, Attempt: 1,
		RequestMethod: "FAKE", RequestURL: reqURL, RequestBody: reqBody,
		ResponseStatus: status, ResponseBody: respBody, ResponseHash: hash,
		SentAt: at, ReceivedAt: at,
	})
	return ref, hash, err
}

func fakeTimeoutErr(op Operation) *errs.Error {
	return withGateway(errs.Wrap(context.DeadlineExceeded, errs.CodeProviderUnavailable, "jupiter: provider unreachable").
		WithField(fieldTimedOut, true).WithField(fieldAttempts, 1+DefaultMaxRetries), op, 0, "")
}

// readFault maps an injected fault for a SAFE_RETRY operation to the error
// the live client would return; ok=false means "no fault".
func (f *Fake) readFault(op Operation) (*errs.Error, bool) {
	switch f.fault(op) {
	case FaultTimeout:
		return fakeTimeoutErr(op), true
	case FaultRateLimited:
		return withGateway(errs.New(errs.CodeRateLimited, "jupiter: rate limited by the provider").
			WithRetryAfter(f.cfg.RetryAfter).WithField(fieldAttempts, 1+DefaultMaxRetries), op, 429, "fake-gw"), true
	case FaultServerError:
		return withGateway(errs.New(errs.CodeProviderUnavailable, "jupiter: provider server error").
			WithField(fieldAttempts, 1+DefaultMaxRetries), op, 503, "fake-gw"), true
	}
	return nil, false
}

// deterministicID derives a hex id from labeled parts.
func deterministicID(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func deterministicHash(seed string) solana.Hash {
	return solana.HashFromBytes(sha256Of([]byte(seed)))
}

// Order implements Service. The response is rendered in the documented
// wire shape and pushed through the same decoder and assembler as the live
// client, so the Fake cannot drift from the wire contract.
func (f *Fake) Order(ctx context.Context, req OrderRequest) (Order, error) {
	q, err := orderQuery(req)
	if err != nil {
		return Order{}, err
	}
	if e, faulted := f.readFault(OpOrder); faulted {
		return Order{}, e
	}
	now := f.cfg.Clock.Now()
	seq := f.nextSeq()
	requestID := deterministicID("jupiter-fake-order", req.InputMint, req.OutputMint, req.Amount.String(), req.TakerPubkey, strconv.FormatUint(seq, 10))
	blockhash := deterministicHash("blockhash|" + requestID)

	slippage := f.cfg.DefaultSlippageBPS
	if req.SlippageBPS != nil {
		slippage = *req.SlippageBPS
	}
	r := f.rate(req.InputMint, req.OutputMint)
	outAmount, err := req.Amount.MulDiv(money.QuantityFromInt64(r.Num), money.QuantityFromInt64(r.Den), money.RoundDown)
	if err != nil {
		return Order{}, errs.Wrap(err, errs.CodeInternal, "jupiter: fake rate")
	}
	threshold, err := outAmount.MulBPSChecked(money.OneHundredPercent-slippage, money.RoundDown)
	if err != nil {
		return Order{}, errs.Wrap(err, errs.CodeInternal, "jupiter: fake threshold")
	}

	f.mu.Lock()
	height := f.blockHeight
	f.mu.Unlock()
	lastValid := height + f.cfg.BlockHeightValidity
	expired := f.fault(OpOrder) == FaultExpired
	if expired && height > 0 {
		lastValid = height - 1
	}

	var txB64 string
	if req.TakerPubkey != "" {
		inAmt, err := req.Amount.Int64()
		if err != nil || inAmt < 0 {
			return Order{}, fieldError("amount", "fake transactions encode amounts as u64")
		}
		outAmt, err := outAmount.Int64()
		if err != nil || outAmt < 0 {
			return Order{}, fieldError("outAmount", "fake transactions encode amounts as u64")
		}
		raw, err := BuildFakeRouteTransaction(FakeRouteParams{
			ProgramID: f.programID, Taker: solana.MustPublicKeyFromBase58(req.TakerPubkey),
			InputMint: solana.MustPublicKeyFromBase58(req.InputMint), OutputMint: solana.MustPublicKeyFromBase58(req.OutputMint),
			InAmount: uint64(inAmt), QuotedOutAmount: uint64(outAmt), SlippageBPS: uint16(slippage), //nolint:gosec // G115: slippage validated 0..10000
			RecentBlockhash: blockhash, ComputeUnitLimit: f.cfg.ComputeUnitLimit, ComputeUnitPriceMicroLamports: f.cfg.ComputeUnitPriceMicroLamports,
		})
		if err != nil {
			return Order{}, errs.Wrap(err, errs.CodeInternal, "jupiter: fake transaction")
		}
		txB64 = base64.StdEncoding.EncodeToString(raw)
	}

	body := f.orderJSON(req, requestID, outAmount, threshold, slippage, txB64, lastValid)
	if f.fault(OpOrder) == FaultInvalidResponse {
		body = []byte(`{"requestId":"` + requestID + `","inAmount":` + req.Amount.String() + `.0,"outAmount":"` + outAmount.String() + `","otherAmountThreshold":"` + threshold.String() + `","slippageBps":` + strconv.FormatInt(int64(slippage), 10) + `}`)
	}
	ref, hash, err := f.archive(ctx, OpOrder, "fake://jupiter/order?"+q.Encode(), nil, body, 200, now)
	if err != nil {
		return Order{}, withGateway(errs.Wrap(err, errs.CodeInternal, "jupiter: evidence archive failed"), OpOrder, 200, "")
	}
	meta := responseMeta{receivedAt: now, rawRef: ref, rawHash: hash, gatewayID: "fake-" + requestID[:8]}
	d, err := decodeOrderResponse(body)
	if err != nil {
		return Order{}, err
	}
	o, err := assembleOrder(req, d, meta, f.cfg.OrderTTL)
	if err != nil {
		return Order{}, err
	}
	if expired {
		o.ExpiresAt = now.Add(-time.Second)
		o.ExpiresAtAssumed = true
	}
	f.mu.Lock()
	f.orders[requestID] = fakeOrder{order: o, blockhash: blockhash}
	f.mu.Unlock()
	return o, nil
}

// orderJSON renders the documented /order response shape.
func (f *Fake) orderJSON(req OrderRequest, requestID string, out, threshold money.Quantity, slippage money.BPS, txB64 string, lastValid uint64) []byte {
	step := map[string]any{
		"swapInfo": map[string]any{
			"ammKey": deterministicID("amm", req.InputMint, req.OutputMint)[:32], "label": "FakeAMM",
			"inputMint": req.InputMint, "outputMint": req.OutputMint,
			"inAmount": req.Amount.String(), "outAmount": out.String(),
		},
		"percent": 100, "bps": 10000, "usdValue": 0,
	}
	body := map[string]any{
		"mode": "manual", "router": "metis", "requestId": requestID,
		"inAmount": req.Amount.String(), "outAmount": out.String(), "otherAmountThreshold": threshold.String(),
		"inUsdValue": 0, "outUsdValue": 0, "swapUsdValue": 0,
		"priceImpact": json.Number(money.QuantityFromInt64(int64(f.cfg.PriceImpactBPS)).ToDecimalString(2)),
		"slippageBps": int64(slippage), "feeBps": int64(f.cfg.FeeBPS),
		"platformFee":          map[string]any{"amount": "0", "feeBps": int64(f.cfg.FeeBPS), "feeMint": req.OutputMint},
		"routePlan":            []any{step},
		"signatureFeeLamports": f.cfg.SignatureFeeLamports, "prioritizationFeeLamports": f.cfg.PrioritizationFeeLamports, "rentFeeLamports": 0,
		"gasless": false, "totalTime": 1,
		"errorCode": nil, "errorMessage": nil, "error": nil,
	}
	if txB64 != "" {
		body["transaction"] = txB64
		body["lastValidBlockHeight"] = strconv.FormatUint(lastValid, 10)
	} else {
		body["transaction"] = nil
	}
	b, err := json.Marshal(body)
	if err != nil {
		// Only strings, integers, json.Number, bool, nil and maps: cannot fail.
		panic("jupiter: fake order json: " + err.Error())
	}
	return b
}

// Build implements Service. Like Order, it renders the documented /build
// shape and decodes it with the production decoder.
func (f *Fake) Build(ctx context.Context, req BuildRequest) (BuildResult, error) {
	q, err := buildQuery(req)
	if err != nil {
		return BuildResult{}, err
	}
	if e, faulted := f.readFault(OpBuild); faulted {
		return BuildResult{}, e
	}
	now := f.cfg.Clock.Now()
	seq := f.nextSeq()
	id := deterministicID("jupiter-fake-build", req.InputMint, req.OutputMint, req.Amount.String(), req.Taker, strconv.FormatUint(seq, 10))
	blockhash := deterministicHash("blockhash|" + id)
	slippage := money.BPS(50)
	if req.SlippageBPS != nil {
		slippage = *req.SlippageBPS
	}
	r := f.rate(req.InputMint, req.OutputMint)
	outAmount, err := req.Amount.MulDiv(money.QuantityFromInt64(r.Num), money.QuantityFromInt64(r.Den), money.RoundDown)
	if err != nil {
		return BuildResult{}, errs.Wrap(err, errs.CodeInternal, "jupiter: fake rate")
	}
	threshold, err := outAmount.MulBPSChecked(money.OneHundredPercent-slippage, money.RoundDown)
	if err != nil {
		return BuildResult{}, errs.Wrap(err, errs.CodeInternal, "jupiter: fake threshold")
	}
	inAmt, err := req.Amount.Int64()
	if err != nil || inAmt < 0 {
		return BuildResult{}, fieldError("amount", "fake instructions encode amounts as u64")
	}
	outAmt, err := outAmount.Int64()
	if err != nil || outAmt < 0 {
		return BuildResult{}, fieldError("outAmount", "fake instructions encode amounts as u64")
	}
	raw, err := BuildFakeRouteTransaction(FakeRouteParams{
		ProgramID: f.programID, Taker: solana.MustPublicKeyFromBase58(req.Taker),
		InputMint: solana.MustPublicKeyFromBase58(req.InputMint), OutputMint: solana.MustPublicKeyFromBase58(req.OutputMint),
		InAmount: uint64(inAmt), QuotedOutAmount: uint64(outAmt), SlippageBPS: uint16(slippage), //nolint:gosec // G115: slippage validated 0..10000
		RecentBlockhash: blockhash, ComputeUnitLimit: f.cfg.ComputeUnitLimit, ComputeUnitPriceMicroLamports: f.cfg.ComputeUnitPriceMicroLamports,
	})
	if err != nil {
		return BuildResult{}, errs.Wrap(err, errs.CodeInternal, "jupiter: fake transaction")
	}
	ixs, err := instructionsJSON(raw)
	if err != nil {
		return BuildResult{}, errs.Wrap(err, errs.CodeInternal, "jupiter: fake instructions")
	}
	f.mu.Lock()
	height := f.blockHeight
	f.mu.Unlock()
	hashBytes := make([]any, 32)
	for i, b := range blockhash[:] {
		hashBytes[i] = int(b)
	}
	body := map[string]any{
		"inAmount": req.Amount.String(), "outAmount": outAmount.String(), "otherAmountThreshold": threshold.String(),
		"slippageBps":    int64(slippage),
		"priceImpactPct": money.QuantityFromInt64(int64(f.cfg.PriceImpactBPS)).ToDecimalString(4),
		"routePlan": []any{map[string]any{
			"swapInfo": map[string]any{
				"ammKey": deterministicID("amm", req.InputMint, req.OutputMint)[:32], "label": "FakeAMM",
				"inputMint": req.InputMint, "outputMint": req.OutputMint,
				"inAmount": req.Amount.String(), "outAmount": outAmount.String(),
			},
			"percent": 100, "bps": 10000,
		}},
		"computeBudgetInstructions":     ixs[:2],
		"setupInstructions":             ixs[2:3],
		"swapInstruction":               ixs[3],
		"cleanupInstruction":            nil,
		"otherInstructions":             []any{},
		"tipInstruction":                nil,
		"addressesByLookupTableAddress": nil,
		"blockhashWithMetadata": map[string]any{
			"blockhash": hashBytes, "lastValidBlockHeight": height + f.cfg.BlockHeightValidity,
			"fetchedAt": map[string]any{"secs_since_epoch": now.Unix(), "nanos_since_epoch": now.Nanosecond()},
		},
	}
	b, err := json.Marshal(body)
	if err != nil {
		return BuildResult{}, errs.Wrap(err, errs.CodeInternal, "jupiter: fake build json")
	}
	if f.fault(OpBuild) == FaultInvalidResponse {
		b = []byte(`{"inAmount":` + req.Amount.String() + `.5}`)
	}
	ref, hash, err := f.archive(ctx, OpBuild, "fake://jupiter/build?"+q.Encode(), nil, b, 200, now)
	if err != nil {
		return BuildResult{}, withGateway(errs.Wrap(err, errs.CodeInternal, "jupiter: evidence archive failed"), OpBuild, 200, "")
	}
	d, err := decodeBuildResponse(b)
	if err != nil {
		return BuildResult{}, err
	}
	return assembleBuild(req, d, responseMeta{receivedAt: now, rawRef: ref, rawHash: hash, gatewayID: "fake-" + id[:8]}, f.cfg.OrderTTL)
}

// instructionsJSON renders the top-level instructions of a serialized
// transaction in the documented /build instruction shape.
func instructionsJSON(raw []byte) ([]any, error) {
	tx, err := solana.TransactionFromBytes(raw)
	if err != nil {
		return nil, err
	}
	msg := &tx.Message
	out := make([]any, 0, len(msg.Instructions))
	for _, ix := range msg.Instructions {
		pid, err := msg.Program(ix.ProgramIDIndex)
		if err != nil {
			return nil, err
		}
		accounts := make([]any, 0, len(ix.Accounts))
		for _, idx := range ix.Accounts {
			if int(idx) >= len(msg.AccountKeys) {
				return nil, errors.New("account index out of range")
			}
			key := msg.AccountKeys[idx]
			signer := int(idx) < int(msg.Header.NumRequiredSignatures)
			writable, err := msg.IsWritable(key)
			if err != nil {
				return nil, err
			}
			accounts = append(accounts, map[string]any{"pubkey": key.String(), "isSigner": signer, "isWritable": writable})
		}
		out = append(out, map[string]any{
			"programId": pid.String(), "accounts": accounts,
			"data": base64.StdEncoding.EncodeToString([]byte(ix.Data)),
		})
	}
	return out, nil
}

// Execute implements Service. Exactly one submission is recorded per call;
// the Fake never retries either.
func (f *Fake) Execute(ctx context.Context, req ExecuteRequest) (ExecuteResult, error) {
	if req.RequestID == "" {
		return ExecuteResult{}, requestError("requestId", "required (Order.QuoteID)")
	}
	summary, err := summarizeTransaction(req.SignedTransaction)
	if err != nil {
		if e, ok := errs.As(err); ok {
			return ExecuteResult{}, e.WithField("field", "signedTransaction").WithField(fieldSubmitted, false)
		}
		return ExecuteResult{}, requestError("signedTransaction", err.Error())
	}
	if summary.numSigned == 0 || summary.firstSignature == "" {
		return ExecuteResult{}, requestError("signedTransaction", "carries no fee-payer signature")
	}
	sig := summary.firstSignature
	if err := ctx.Err(); err != nil {
		return ExecuteResult{}, withGateway(errs.Wrap(err, errs.CodeProviderUnavailable, "jupiter: context done before submission").
			WithField(fieldSubmitted, false).WithField(fieldSignature, sig), OpExecute, 0, "")
	}
	now := f.cfg.Clock.Now()
	res := ExecuteResult{Signature: sig, SubmittedAt: now, ReceivedAt: now, GatewayRequestID: "fake-exec"}

	switch f.fault(OpExecute) {
	case FaultRateLimited:
		return res, withGateway(errs.New(errs.CodeRateLimited, "jupiter: rate limited by the provider; the transaction was not accepted").
			WithRetryAfter(f.cfg.RetryAfter).WithField(fieldSubmitted, false), OpExecute, 429, "fake-gw")
	case FaultExpired:
		return res, withGateway(errs.New(errs.CodeQuoteExpired, "jupiter: the order is no longer cached by the provider; re-quote").
			WithField(fieldProviderCode, executeCodeMissingCachedOrder).WithField(fieldSubmitted, false), OpExecute, 400, "fake-gw")
	case FaultServerError:
		return res, submissionUnknown(errors.New("500 {signature, error}"), "jupiter: provider server error during submission; landing state unknown", sig, "fake-gw", false)
	case FaultInvalidResponse:
		return res, submissionUnknown(errors.New("undecodable body"), "jupiter: submission response could not be decoded; landing state unknown", sig, "fake-gw", false)
	}

	f.mu.Lock()
	rec, known := f.orders[req.RequestID]
	height := f.blockHeight
	f.mu.Unlock()
	if !known {
		return res, withGateway(errs.New(errs.CodeQuoteExpired, "jupiter: the order is no longer cached by the provider; re-quote").
			WithField(fieldProviderCode, executeCodeMissingCachedOrder).WithField(fieldSubmitted, false), OpExecute, 400, "fake-gw")
	}
	if summary.recentBlockhash != rec.blockhash.String() || summary.feePayer != rec.order.Taker {
		return res, withGateway(errs.New(errs.CodeValidationFailed, "jupiter: the provider rejected the signed transaction bytes").
			WithField(fieldProviderCode, executeCodeInvalidSignedTx).WithField(fieldSubmitted, false), OpExecute, 400, "fake-gw")
	}

	body := map[string]any{
		"signature": sig, "slot": strconv.FormatUint(height+1, 10), "code": 0, "error": nil,
		"totalInputAmount": rec.order.InAmount.String(), "totalOutputAmount": rec.order.OutAmount.String(),
		"inputAmountResult": rec.order.InAmount.String(), "outputAmountResult": rec.order.OutAmount.String(),
		"swapEvents": []any{map[string]any{
			"inputMint": rec.order.InputMint, "inputAmount": rec.order.InAmount.String(),
			"outputMint": rec.order.OutputMint, "outputAmount": rec.order.OutAmount.String(),
		}},
	}
	sub := FakeSubmission{RequestID: req.RequestID, Signature: sig, Slot: height + 1, At: now}
	switch {
	case f.fault(OpExecute) == FaultSlippageExceeded:
		// -1001 is inside the documented aggregator failure range; the exact
		// meaning of each code is undocumented, so this is fake-only.
		body["status"], body["code"], body["error"] = string(ExecuteFailed), -1001, "slippage tolerance exceeded"
		body["signature"], body["slot"] = sig, "0"
		sub.Status = ExecuteFailed
	case height > rec.order.LastValidBlockHeight:
		body["status"], body["code"], body["error"] = string(ExecuteFailed), -1000, "block height exceeded"
		body["slot"] = "0"
		sub.Status = ExecuteFailed
	default:
		body["status"] = string(ExecuteSuccess)
		sub.Status = ExecuteSuccess
	}
	b, err := json.Marshal(body)
	if err != nil {
		return res, submissionUnknown(err, "jupiter: fake execute json", sig, "fake-gw", false)
	}
	ref, hash, err := f.archive(ctx, OpExecute, "fake://jupiter/execute", []byte(`{"requestId":"`+req.RequestID+`"}`), b, 200, now)
	res.RawRef, res.RawHash = ref, hash
	if err != nil {
		return res, submissionUnknown(err, "jupiter: evidence archive failed after submission; landing state unknown", sig, "fake-gw", false)
	}
	timeoutFault := f.fault(OpExecute) == FaultTimeout
	if sub.Status == ExecuteSuccess {
		sub.ResponseLost = timeoutFault
		f.mu.Lock()
		f.submissions = append(f.submissions, sub)
		f.mu.Unlock()
	}
	if timeoutFault {
		return res, submissionUnknown(context.DeadlineExceeded, "jupiter: no response to the submission; landing state unknown", sig, "fake-gw", true)
	}
	d, err := decodeExecuteResponse(b)
	if err != nil {
		return res, submissionUnknown(err, "jupiter: submission response could not be decoded; landing state unknown", sig, "fake-gw", false)
	}
	res.Status, res.Slot, res.ProviderCode, res.ProviderError = ExecuteStatus(d.status), d.slot, d.code, d.errText
	res.TotalInputAmount, res.TotalOutputAmount = d.totalInputAmount, d.totalOutputAmount
	res.InputAmountResult, res.OutputAmountResult = d.inputAmountResult, d.outputAmountResult
	res.SwapEvents = d.swapEvents
	return res, nil
}
