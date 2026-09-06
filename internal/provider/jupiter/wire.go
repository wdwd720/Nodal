package jupiter

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

// Wire types for the Swap API V2. Every field is decoded as a raw token and
// interpreted by the helpers in numbers.go so that the error for any
// malformed field names the field. Nothing in this file is exported.
//
// Source of every field: docs/api/providers/jupiter.md, section "Endpoints
// and schemas (Swap API V2)", which mirrors the OpenAPI specification at
// https://developers.jup.ag/docs/openapi-spec/swap/v2/swap.yaml.

// orderWire is the GET /order 200 body.
type orderWire struct {
	Mode                      json.RawMessage `json:"mode"`
	InAmount                  json.RawMessage `json:"inAmount"`
	OutAmount                 json.RawMessage `json:"outAmount"`
	OtherAmountThreshold      json.RawMessage `json:"otherAmountThreshold"`
	PriceImpact               json.RawMessage `json:"priceImpact"`
	PriceImpactPct            json.RawMessage `json:"priceImpactPct"` // deprecated on /order; never used
	SlippageBps               json.RawMessage `json:"slippageBps"`
	FeeBps                    json.RawMessage `json:"feeBps"`
	PlatformFee               json.RawMessage `json:"platformFee"`
	RoutePlan                 json.RawMessage `json:"routePlan"`
	Router                    json.RawMessage `json:"router"`
	Transaction               json.RawMessage `json:"transaction"`
	LastValidBlockHeight      json.RawMessage `json:"lastValidBlockHeight"`
	ExpireAt                  json.RawMessage `json:"expireAt"`
	QuoteID                   json.RawMessage `json:"quoteId"`
	Maker                     json.RawMessage `json:"maker"`
	SignatureFeeLamports      json.RawMessage `json:"signatureFeeLamports"`
	PrioritizationFeeLamports json.RawMessage `json:"prioritizationFeeLamports"`
	RentFeeLamports           json.RawMessage `json:"rentFeeLamports"`
	Gasless                   json.RawMessage `json:"gasless"`
	RequestID                 json.RawMessage `json:"requestId"`
	ErrorCode                 json.RawMessage `json:"errorCode"`
	ErrorMessage              json.RawMessage `json:"errorMessage"`
	Error                     json.RawMessage `json:"error"`
}

type platformFeeWire struct {
	Amount  json.RawMessage `json:"amount"`
	FeeBps  json.RawMessage `json:"feeBps"`
	FeeMint json.RawMessage `json:"feeMint"`
}

type routeStepWire struct {
	SwapInfo struct {
		AmmKey     json.RawMessage `json:"ammKey"`
		Label      json.RawMessage `json:"label"`
		InputMint  json.RawMessage `json:"inputMint"`
		OutputMint json.RawMessage `json:"outputMint"`
		InAmount   json.RawMessage `json:"inAmount"`
		OutAmount  json.RawMessage `json:"outAmount"`
	} `json:"swapInfo"`
	Bps json.RawMessage `json:"bps"`
	// percent and usdValue are informational numbers; they are hashed as
	// part of the route summary but never interpreted.
}

// decodedOrder is the interpreted /order body, still internal.
type decodedOrder struct {
	requestID string
	mode      string
	router    string

	inAmount             money.Quantity
	outAmount            money.Quantity
	otherAmountThreshold money.Quantity
	slippageBPS          money.BPS
	feeBPS               money.BPS
	platformFee          *PlatformFee

	priceImpactBPS money.BPS
	priceImpactOK  bool

	route        []RouteStep
	routeSummary json.RawMessage
	routeHash    []byte

	transaction    []byte
	hasTransaction bool

	lastValidBlockHeight    uint64
	lastValidBlockHeightRaw string

	expireAt   time.Time
	rfqQuoteID string
	maker      string

	signatureFeeLamports      money.Quantity
	prioritizationFeeLamports money.Quantity
	rentFeeLamports           money.Quantity
	gasless                   bool

	errorCode    int64
	errorMessage string
}

// decodeOrderResponse strictly decodes a 200 /order body. It never panics.
// A non-zero errorCode short-circuits: the returned value carries only the
// error fields and requestId, and the caller maps it to a typed error.
func decodeOrderResponse(body []byte) (decodedOrder, error) {
	var w orderWire
	if err := json.Unmarshal(body, &w); err != nil {
		return decodedOrder{}, errs.Wrap(err, errs.CodeValidationFailed, "jupiter: order response is not a JSON object")
	}
	var d decodedOrder
	var err error

	// Error fields first: a build error still returns 200 with an empty
	// transaction and errorCode set.
	if d.errorCode, err = optionalIntegerNumberField(w.ErrorCode, "errorCode"); err != nil {
		return decodedOrder{}, err
	}
	if d.errorMessage, err = optionalStringField(w.ErrorMessage, "errorMessage"); err != nil {
		return decodedOrder{}, err
	}
	if d.errorMessage == "" {
		if d.errorMessage, err = optionalStringField(w.Error, "error"); err != nil {
			return decodedOrder{}, err
		}
	}
	if d.requestID, err = optionalStringField(w.RequestID, "requestId"); err != nil {
		return decodedOrder{}, err
	}
	if d.router, err = optionalStringField(w.Router, "router"); err != nil {
		return decodedOrder{}, err
	}
	if d.errorCode != 0 {
		return d, nil
	}

	if d.requestID == "" {
		return decodedOrder{}, fieldError("requestId", "missing required field")
	}
	if d.mode, err = optionalStringField(w.Mode, "mode"); err != nil {
		return decodedOrder{}, err
	}
	if d.inAmount, err = quantityStringField(w.InAmount, "inAmount"); err != nil {
		return decodedOrder{}, err
	}
	if d.outAmount, err = quantityStringField(w.OutAmount, "outAmount"); err != nil {
		return decodedOrder{}, err
	}
	if d.otherAmountThreshold, err = quantityStringField(w.OtherAmountThreshold, "otherAmountThreshold"); err != nil {
		return decodedOrder{}, err
	}
	if d.slippageBPS, err = bpsField(w.SlippageBps, "slippageBps"); err != nil {
		return decodedOrder{}, err
	}
	if d.feeBPS, err = optionalBPSField(w.FeeBps, "feeBps"); err != nil {
		return decodedOrder{}, err
	}
	if d.platformFee, err = decodePlatformFee(w.PlatformFee); err != nil {
		return decodedOrder{}, err
	}
	if d.priceImpactBPS, d.priceImpactOK, err = priceImpactFromNumber(w.PriceImpact, "priceImpact"); err != nil {
		return decodedOrder{}, err
	}
	if d.route, err = decodeRoutePlan(w.RoutePlan); err != nil {
		return decodedOrder{}, err
	}
	if d.routeSummary, d.routeHash, err = routeSummary(w.RoutePlan); err != nil {
		return decodedOrder{}, errs.Wrap(err, errs.CodeValidationFailed, "jupiter: routePlan: not canonicalizable").WithField("field", "routePlan")
	}

	// transaction: string (base64), null (no taker) or "" (build error).
	txB64, err := optionalStringField(w.Transaction, "transaction")
	if err != nil {
		return decodedOrder{}, err
	}
	if txB64 != "" {
		raw, decErr := base64.StdEncoding.DecodeString(txB64)
		if decErr != nil {
			return decodedOrder{}, fieldError("transaction", "not valid base64")
		}
		if len(raw) == 0 {
			return decodedOrder{}, fieldError("transaction", "decodes to zero bytes")
		}
		if len(raw) > MaxTransactionBytes {
			return decodedOrder{}, fieldError("transaction", "exceeds the 1232-byte transaction limit")
		}
		d.transaction = raw
		d.hasTransaction = true
	}

	// lastValidBlockHeight: string; required whenever a transaction exists
	// (it is the hard expiry), optional for quote-only responses.
	if d.hasTransaction {
		if d.lastValidBlockHeight, err = uint64StringField(w.LastValidBlockHeight, "lastValidBlockHeight"); err != nil {
			return decodedOrder{}, err
		}
		if d.lastValidBlockHeight == 0 {
			return decodedOrder{}, fieldError("lastValidBlockHeight", "must be positive")
		}
	} else if d.lastValidBlockHeight, err = optionalUint64StringField(w.LastValidBlockHeight, "lastValidBlockHeight"); err != nil {
		return decodedOrder{}, err
	}
	if !isNull(w.LastValidBlockHeight) {
		if d.lastValidBlockHeightRaw, err = optionalStringField(w.LastValidBlockHeight, "lastValidBlockHeight"); err != nil {
			return decodedOrder{}, err
		}
	}

	expireAt, err := optionalStringField(w.ExpireAt, "expireAt")
	if err != nil {
		return decodedOrder{}, err
	}
	if d.expireAt, err = parseExpireAt(expireAt); err != nil {
		return decodedOrder{}, err
	}
	if d.rfqQuoteID, err = optionalStringField(w.QuoteID, "quoteId"); err != nil {
		return decodedOrder{}, err
	}
	if d.maker, err = optionalStringField(w.Maker, "maker"); err != nil {
		return decodedOrder{}, err
	}

	if d.signatureFeeLamports, err = optionalQuantityNumberField(w.SignatureFeeLamports, "signatureFeeLamports"); err != nil {
		return decodedOrder{}, err
	}
	if d.prioritizationFeeLamports, err = optionalQuantityNumberField(w.PrioritizationFeeLamports, "prioritizationFeeLamports"); err != nil {
		return decodedOrder{}, err
	}
	if d.rentFeeLamports, err = optionalQuantityNumberField(w.RentFeeLamports, "rentFeeLamports"); err != nil {
		return decodedOrder{}, err
	}
	if d.gasless, err = optionalBoolField(w.Gasless, "gasless"); err != nil {
		return decodedOrder{}, err
	}
	return d, nil
}

func decodePlatformFee(raw json.RawMessage) (*PlatformFee, error) {
	if isNull(raw) {
		return nil, nil //nolint:nilnil // absent platform fee is legitimate
	}
	if tokenKind(raw) != "object" {
		return nil, fieldError("platformFee", "must be a JSON object, got "+tokenKind(raw))
	}
	var w platformFeeWire
	if err := json.Unmarshal(raw, &w); err != nil {
		return nil, fieldError("platformFee", "invalid JSON object")
	}
	amount, err := quantityStringField(w.Amount, "platformFee.amount")
	if err != nil {
		return nil, err
	}
	feeBPS, err := bpsField(w.FeeBps, "platformFee.feeBps")
	if err != nil {
		return nil, err
	}
	mint, err := optionalStringField(w.FeeMint, "platformFee.feeMint")
	if err != nil {
		return nil, err
	}
	return &PlatformFee{Amount: amount, FeeBPS: feeBPS, FeeMint: mint}, nil
}

// decodeRoutePlan interprets routePlan[]. An absent plan yields nil.
func decodeRoutePlan(raw json.RawMessage) ([]RouteStep, error) {
	if isNull(raw) {
		return nil, nil
	}
	if tokenKind(raw) != "array" {
		return nil, fieldError("routePlan", "must be a JSON array, got "+tokenKind(raw))
	}
	var steps []routeStepWire
	if err := json.Unmarshal(raw, &steps); err != nil {
		return nil, fieldError("routePlan", "invalid JSON array of steps")
	}
	out := make([]RouteStep, 0, len(steps))
	for i, s := range steps {
		prefix := "routePlan[" + itoa(i) + "].swapInfo."
		var step RouteStep
		var err error
		if step.AMMKey, err = optionalStringField(s.SwapInfo.AmmKey, prefix+"ammKey"); err != nil {
			return nil, err
		}
		if step.Label, err = optionalStringField(s.SwapInfo.Label, prefix+"label"); err != nil {
			return nil, err
		}
		if step.InputMint, err = stringField(s.SwapInfo.InputMint, prefix+"inputMint"); err != nil {
			return nil, err
		}
		if step.OutputMint, err = stringField(s.SwapInfo.OutputMint, prefix+"outputMint"); err != nil {
			return nil, err
		}
		if step.InAmount, err = quantityStringField(s.SwapInfo.InAmount, prefix+"inAmount"); err != nil {
			return nil, err
		}
		if step.OutAmount, err = quantityStringField(s.SwapInfo.OutAmount, prefix+"outAmount"); err != nil {
			return nil, err
		}
		if step.BPS, err = optionalIntegerNumberField(s.Bps, "routePlan["+itoa(i)+"].bps"); err != nil {
			return nil, err
		}
		out = append(out, step)
	}
	return out, nil
}

// executeWire is the POST /execute 200 body.
type executeWire struct {
	Status             json.RawMessage `json:"status"`
	Signature          json.RawMessage `json:"signature"`
	Slot               json.RawMessage `json:"slot"`
	Error              json.RawMessage `json:"error"`
	Code               json.RawMessage `json:"code"`
	TotalInputAmount   json.RawMessage `json:"totalInputAmount"`
	TotalOutputAmount  json.RawMessage `json:"totalOutputAmount"`
	InputAmountResult  json.RawMessage `json:"inputAmountResult"`
	OutputAmountResult json.RawMessage `json:"outputAmountResult"`
	SwapEvents         json.RawMessage `json:"swapEvents"`
}

type swapEventWire struct {
	InputMint    json.RawMessage `json:"inputMint"`
	InputAmount  json.RawMessage `json:"inputAmount"`
	OutputMint   json.RawMessage `json:"outputMint"`
	OutputAmount json.RawMessage `json:"outputAmount"`
}

// decodedExecute is the interpreted /execute 200 body.
type decodedExecute struct {
	status    string
	signature string
	slot      uint64
	errText   string
	code      int64

	totalInputAmount   *money.Quantity
	totalOutputAmount  *money.Quantity
	inputAmountResult  *money.Quantity
	outputAmountResult *money.Quantity
	swapEvents         []SwapEvent
}

// decodeExecuteResponse strictly decodes a 200 /execute body.
func decodeExecuteResponse(body []byte) (decodedExecute, error) {
	var w executeWire
	if err := json.Unmarshal(body, &w); err != nil {
		return decodedExecute{}, errs.Wrap(err, errs.CodeValidationFailed, "jupiter: execute response is not a JSON object")
	}
	var d decodedExecute
	var err error
	if d.status, err = stringField(w.Status, "status"); err != nil {
		return decodedExecute{}, err
	}
	if d.status == "" {
		return decodedExecute{}, fieldError("status", "must not be empty")
	}
	if d.signature, err = optionalStringField(w.Signature, "signature"); err != nil {
		return decodedExecute{}, err
	}
	if d.slot, err = optionalUint64StringField(w.Slot, "slot"); err != nil {
		return decodedExecute{}, err
	}
	if d.errText, err = optionalStringField(w.Error, "error"); err != nil {
		return decodedExecute{}, err
	}
	if d.code, err = optionalIntegerNumberField(w.Code, "code"); err != nil {
		return decodedExecute{}, err
	}
	if d.totalInputAmount, err = optionalQuantityPtrField(w.TotalInputAmount, "totalInputAmount"); err != nil {
		return decodedExecute{}, err
	}
	if d.totalOutputAmount, err = optionalQuantityPtrField(w.TotalOutputAmount, "totalOutputAmount"); err != nil {
		return decodedExecute{}, err
	}
	if d.inputAmountResult, err = optionalQuantityPtrField(w.InputAmountResult, "inputAmountResult"); err != nil {
		return decodedExecute{}, err
	}
	if d.outputAmountResult, err = optionalQuantityPtrField(w.OutputAmountResult, "outputAmountResult"); err != nil {
		return decodedExecute{}, err
	}
	if !isNull(w.SwapEvents) {
		if tokenKind(w.SwapEvents) != "array" {
			return decodedExecute{}, fieldError("swapEvents", "must be a JSON array, got "+tokenKind(w.SwapEvents))
		}
		var events []swapEventWire
		if err := json.Unmarshal(w.SwapEvents, &events); err != nil {
			return decodedExecute{}, fieldError("swapEvents", "invalid JSON array")
		}
		for i, e := range events {
			prefix := "swapEvents[" + itoa(i) + "]."
			var ev SwapEvent
			if ev.InputMint, err = stringField(e.InputMint, prefix+"inputMint"); err != nil {
				return decodedExecute{}, err
			}
			if ev.InputAmount, err = quantityStringField(e.InputAmount, prefix+"inputAmount"); err != nil {
				return decodedExecute{}, err
			}
			if ev.OutputMint, err = stringField(e.OutputMint, prefix+"outputMint"); err != nil {
				return decodedExecute{}, err
			}
			if ev.OutputAmount, err = quantityStringField(e.OutputAmount, prefix+"outputAmount"); err != nil {
				return decodedExecute{}, err
			}
			d.swapEvents = append(d.swapEvents, ev)
		}
	}
	return d, nil
}

// buildWire is the GET /build 200 body.
type buildWire struct {
	InAmount                      json.RawMessage `json:"inAmount"`
	OutAmount                     json.RawMessage `json:"outAmount"`
	OtherAmountThreshold          json.RawMessage `json:"otherAmountThreshold"`
	SlippageBps                   json.RawMessage `json:"slippageBps"`
	PriceImpactPct                json.RawMessage `json:"priceImpactPct"`
	RoutePlan                     json.RawMessage `json:"routePlan"`
	ComputeBudgetInstructions     json.RawMessage `json:"computeBudgetInstructions"`
	SetupInstructions             json.RawMessage `json:"setupInstructions"`
	SwapInstruction               json.RawMessage `json:"swapInstruction"`
	CleanupInstruction            json.RawMessage `json:"cleanupInstruction"`
	OtherInstructions             json.RawMessage `json:"otherInstructions"`
	TipInstruction                json.RawMessage `json:"tipInstruction"`
	AddressesByLookupTableAddress json.RawMessage `json:"addressesByLookupTableAddress"`
	BlockhashWithMetadata         json.RawMessage `json:"blockhashWithMetadata"`
}

type instructionWire struct {
	ProgramID json.RawMessage `json:"programId"`
	Accounts  json.RawMessage `json:"accounts"`
	Data      json.RawMessage `json:"data"`
}

type accountMetaWire struct {
	Pubkey     json.RawMessage `json:"pubkey"`
	IsSigner   json.RawMessage `json:"isSigner"`
	IsWritable json.RawMessage `json:"isWritable"`
}

type blockhashMetaWire struct {
	Blockhash            json.RawMessage `json:"blockhash"`
	LastValidBlockHeight json.RawMessage `json:"lastValidBlockHeight"`
	FetchedAt            struct {
		Secs  json.RawMessage `json:"secs_since_epoch"`
		Nanos json.RawMessage `json:"nanos_since_epoch"`
	} `json:"fetchedAt"`
}

// decodedBuild is the interpreted /build body.
type decodedBuild struct {
	inAmount             money.Quantity
	outAmount            money.Quantity
	otherAmountThreshold money.Quantity
	slippageBPS          money.BPS
	priceImpactBPS       money.BPS
	priceImpactOK        bool

	route        []RouteStep
	routeSummary json.RawMessage
	routeHash    []byte

	computeBudget []Instruction
	setup         []Instruction
	swap          Instruction
	cleanup       *Instruction
	other         []Instruction
	tip           *Instruction
	lookupTables  map[string][]string

	blockhash            string
	lastValidBlockHeight uint64
	fetchedAt            time.Time
}

// decodeBuildResponse strictly decodes a 200 /build body.
func decodeBuildResponse(body []byte) (decodedBuild, error) {
	var w buildWire
	if err := json.Unmarshal(body, &w); err != nil {
		return decodedBuild{}, errs.Wrap(err, errs.CodeValidationFailed, "jupiter: build response is not a JSON object")
	}
	var d decodedBuild
	var err error
	if d.inAmount, err = quantityStringField(w.InAmount, "inAmount"); err != nil {
		return decodedBuild{}, err
	}
	if d.outAmount, err = quantityStringField(w.OutAmount, "outAmount"); err != nil {
		return decodedBuild{}, err
	}
	if d.otherAmountThreshold, err = quantityStringField(w.OtherAmountThreshold, "otherAmountThreshold"); err != nil {
		return decodedBuild{}, err
	}
	if d.slippageBPS, err = bpsField(w.SlippageBps, "slippageBps"); err != nil {
		return decodedBuild{}, err
	}
	if d.priceImpactBPS, d.priceImpactOK, err = priceImpactFromFractionString(w.PriceImpactPct, "priceImpactPct"); err != nil {
		return decodedBuild{}, err
	}
	if d.route, err = decodeRoutePlan(w.RoutePlan); err != nil {
		return decodedBuild{}, err
	}
	if d.routeSummary, d.routeHash, err = routeSummary(w.RoutePlan); err != nil {
		return decodedBuild{}, errs.Wrap(err, errs.CodeValidationFailed, "jupiter: routePlan: not canonicalizable").WithField("field", "routePlan")
	}
	if d.computeBudget, err = decodeInstructionList(w.ComputeBudgetInstructions, "computeBudgetInstructions"); err != nil {
		return decodedBuild{}, err
	}
	if d.setup, err = decodeInstructionList(w.SetupInstructions, "setupInstructions"); err != nil {
		return decodedBuild{}, err
	}
	if isNull(w.SwapInstruction) {
		return decodedBuild{}, fieldError("swapInstruction", "missing required field")
	}
	if d.swap, err = decodeInstruction(w.SwapInstruction, "swapInstruction"); err != nil {
		return decodedBuild{}, err
	}
	if d.cleanup, err = decodeOptionalInstruction(w.CleanupInstruction, "cleanupInstruction"); err != nil {
		return decodedBuild{}, err
	}
	if d.other, err = decodeInstructionList(w.OtherInstructions, "otherInstructions"); err != nil {
		return decodedBuild{}, err
	}
	if d.tip, err = decodeOptionalInstruction(w.TipInstruction, "tipInstruction"); err != nil {
		return decodedBuild{}, err
	}
	if d.lookupTables, err = decodeLookupTables(w.AddressesByLookupTableAddress); err != nil {
		return decodedBuild{}, err
	}
	if d.blockhash, d.lastValidBlockHeight, d.fetchedAt, err = decodeBlockhashMeta(w.BlockhashWithMetadata); err != nil {
		return decodedBuild{}, err
	}
	return d, nil
}

func decodeInstructionList(raw json.RawMessage, field string) ([]Instruction, error) {
	if isNull(raw) {
		return nil, nil
	}
	if tokenKind(raw) != "array" {
		return nil, fieldError(field, "must be a JSON array, got "+tokenKind(raw))
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fieldError(field, "invalid JSON array")
	}
	out := make([]Instruction, 0, len(items))
	for i, item := range items {
		ix, err := decodeInstruction(item, field+"["+itoa(i)+"]")
		if err != nil {
			return nil, err
		}
		out = append(out, ix)
	}
	return out, nil
}

func decodeOptionalInstruction(raw json.RawMessage, field string) (*Instruction, error) {
	if isNull(raw) {
		return nil, nil //nolint:nilnil // nullable instruction slot
	}
	ix, err := decodeInstruction(raw, field)
	if err != nil {
		return nil, err
	}
	return &ix, nil
}

func decodeInstruction(raw json.RawMessage, field string) (Instruction, error) {
	if tokenKind(raw) != "object" {
		return Instruction{}, fieldError(field, "must be a JSON object, got "+tokenKind(raw))
	}
	var w instructionWire
	if err := json.Unmarshal(raw, &w); err != nil {
		return Instruction{}, fieldError(field, "invalid JSON object")
	}
	var ix Instruction
	var err error
	if ix.ProgramID, err = stringField(w.ProgramID, field+".programId"); err != nil {
		return Instruction{}, err
	}
	dataB64, err := stringField(w.Data, field+".data")
	if err != nil {
		return Instruction{}, err
	}
	if ix.Data, err = base64.StdEncoding.DecodeString(dataB64); err != nil {
		return Instruction{}, fieldError(field+".data", "not valid base64")
	}
	if !isNull(w.Accounts) {
		if tokenKind(w.Accounts) != "array" {
			return Instruction{}, fieldError(field+".accounts", "must be a JSON array")
		}
		var metas []accountMetaWire
		if err := json.Unmarshal(w.Accounts, &metas); err != nil {
			return Instruction{}, fieldError(field+".accounts", "invalid JSON array")
		}
		for i, m := range metas {
			prefix := field + ".accounts[" + itoa(i) + "]."
			var am AccountMeta
			if am.Pubkey, err = stringField(m.Pubkey, prefix+"pubkey"); err != nil {
				return Instruction{}, err
			}
			if am.IsSigner, err = optionalBoolField(m.IsSigner, prefix+"isSigner"); err != nil {
				return Instruction{}, err
			}
			if am.IsWritable, err = optionalBoolField(m.IsWritable, prefix+"isWritable"); err != nil {
				return Instruction{}, err
			}
			ix.Accounts = append(ix.Accounts, am)
		}
	}
	return ix, nil
}

func decodeLookupTables(raw json.RawMessage) (map[string][]string, error) {
	if isNull(raw) {
		return nil, nil
	}
	if tokenKind(raw) != "object" {
		return nil, fieldError("addressesByLookupTableAddress", "must be a JSON object, got "+tokenKind(raw))
	}
	var m map[string][]string
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fieldError("addressesByLookupTableAddress", "must map addresses to string arrays")
	}
	return m, nil
}

// decodeBlockhashMeta reads blockhashWithMetadata: blockhash as a 32-entry
// byte array (JSON numbers), lastValidBlockHeight number, fetchedAt
// {secs_since_epoch, nanos_since_epoch}.
func decodeBlockhashMeta(raw json.RawMessage) (string, uint64, time.Time, error) {
	if isNull(raw) {
		return "", 0, time.Time{}, fieldError("blockhashWithMetadata", "missing required field")
	}
	if tokenKind(raw) != "object" {
		return "", 0, time.Time{}, fieldError("blockhashWithMetadata", "must be a JSON object")
	}
	var w blockhashMetaWire
	if err := json.Unmarshal(raw, &w); err != nil {
		return "", 0, time.Time{}, fieldError("blockhashWithMetadata", "invalid JSON object")
	}
	if tokenKind(w.Blockhash) != "array" {
		return "", 0, time.Time{}, fieldError("blockhashWithMetadata.blockhash", "must be a JSON array of bytes")
	}
	var nums []json.RawMessage
	if err := json.Unmarshal(w.Blockhash, &nums); err != nil {
		return "", 0, time.Time{}, fieldError("blockhashWithMetadata.blockhash", "invalid byte array")
	}
	if len(nums) != 32 {
		return "", 0, time.Time{}, fieldError("blockhashWithMetadata.blockhash", "must have exactly 32 bytes")
	}
	hashBytes := make([]byte, 32)
	for i, n := range nums {
		v, err := integerNumberField(n, "blockhashWithMetadata.blockhash["+itoa(i)+"]")
		if err != nil {
			return "", 0, time.Time{}, err
		}
		if v < 0 || v > 255 {
			return "", 0, time.Time{}, fieldError("blockhashWithMetadata.blockhash["+itoa(i)+"]", "byte out of range")
		}
		hashBytes[i] = byte(v)
	}
	lvbh, err := integerNumberField(w.LastValidBlockHeight, "blockhashWithMetadata.lastValidBlockHeight")
	if err != nil {
		return "", 0, time.Time{}, err
	}
	if lvbh <= 0 {
		return "", 0, time.Time{}, fieldError("blockhashWithMetadata.lastValidBlockHeight", "must be positive")
	}
	secs, err := optionalIntegerNumberField(w.FetchedAt.Secs, "blockhashWithMetadata.fetchedAt.secs_since_epoch")
	if err != nil {
		return "", 0, time.Time{}, err
	}
	nanos, err := optionalIntegerNumberField(w.FetchedAt.Nanos, "blockhashWithMetadata.fetchedAt.nanos_since_epoch")
	if err != nil {
		return "", 0, time.Time{}, err
	}
	if nanos < 0 || nanos > 999_999_999 {
		return "", 0, time.Time{}, fieldError("blockhashWithMetadata.fetchedAt.nanos_since_epoch", "out of range")
	}
	var fetched time.Time
	if secs > 0 {
		fetched = time.Unix(secs, nanos).UTC()
	}
	return base58Encode(hashBytes), uint64(lvbh), fetched, nil
}

// errorBody is the lenient projection of a non-2xx body. Documented shapes:
// /order 400 {requestId, error}; /build 400 {error}; /execute 400 {error,
// code}; /execute 500 {signature, error}; 429 plain text.
type errorBody struct {
	requestID string
	message   string
	code      *int64
	signature string
	// raw is a bounded copy of the body for logs when it is not JSON.
	raw string
}

const maxErrorBodyEcho = 512

func decodeErrorBody(body []byte) errorBody {
	var out errorBody
	trimmed := strings.TrimSpace(string(body))
	if len(trimmed) > maxErrorBodyEcho {
		trimmed = trimmed[:maxErrorBodyEcho]
	}
	out.raw = trimmed
	var w struct {
		RequestID json.RawMessage `json:"requestId"`
		Error     json.RawMessage `json:"error"`
		Message   json.RawMessage `json:"message"`
		Code      json.RawMessage `json:"code"`
		Signature json.RawMessage `json:"signature"`
	}
	if err := json.Unmarshal(body, &w); err != nil {
		return out
	}
	out.requestID, _ = optionalStringField(w.RequestID, "requestId")
	out.message, _ = optionalStringField(w.Error, "error")
	if out.message == "" {
		out.message, _ = optionalStringField(w.Message, "message")
	}
	out.signature, _ = optionalStringField(w.Signature, "signature")
	if !isNull(w.Code) {
		if c, err := optionalIntegerNumberField(w.Code, "code"); err == nil {
			out.code = &c
		}
	}
	return out
}

func itoa(i int) string {
	const digits = "0123456789"
	if i == 0 {
		return "0"
	}
	var b [20]byte
	n := len(b)
	neg := i < 0
	if neg {
		i = -i
	}
	for i > 0 {
		n--
		b[n] = digits[i%10]
		i /= 10
	}
	if neg {
		n--
		b[n] = '-'
	}
	return string(b[n:])
}
