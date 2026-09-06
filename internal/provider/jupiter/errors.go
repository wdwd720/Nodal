package jupiter

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/nodal/controlplane/internal/errs"
)

// Error mapping. Every function here returns *errs.Error with a stable
// code; Detail is client-safe and the provider's message goes into Fields.

// Field keys used in mapped errors.
const (
	fieldProviderError     = "provider_error"
	fieldProviderCode      = "provider_code"
	fieldProviderRequestID = "provider_request_id"
	fieldGatewayRequestID  = "gateway_request_id"
	fieldHTTPStatus        = "http_status"
	fieldOperation         = "operation"
	fieldSignature         = "signature"
	fieldSubmitted         = "submitted"
	fieldTimedOut          = "timed_out"
	fieldReason            = "reason"
	fieldAttempts          = "attempts"
)

// isTimeout reports whether err is a deadline/timeout of any kind.
func isTimeout(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	return false
}

// liquidityPatterns classify a free-text /order 400 message as "no route /
// insufficient liquidity". UNVERIFIED: the responses page does not
// formalize the JSON error body, so this is a conservative substring
// heuristic; anything unmatched stays VALIDATION_FAILED.
var liquidityPatterns = []string{
	"no route", "route not found", "could not find any route", "no routes found",
	"insufficient liquidity", "not enough liquidity", "liquidity",
}

// expiryPatterns classify a free-text message as an expired/missing order.
// UNVERIFIED for the same reason.
var expiryPatterns = []string{"expired", "blockhash not found", "block height exceeded", "not found in cache", "missing cached order"}

func matchesAny(msg string, patterns []string) bool {
	m := strings.ToLower(msg)
	for _, p := range patterns {
		if strings.Contains(m, p) {
			return true
		}
	}
	return false
}

// withGateway attaches gateway and operation fields.
func withGateway(e *errs.Error, op Operation, status int, gatewayID string) *errs.Error {
	e = e.WithField(fieldOperation, string(op))
	if status != 0 {
		e = e.WithField(fieldHTTPStatus, status)
	}
	if gatewayID != "" {
		e = e.WithField(fieldGatewayRequestID, gatewayID)
	}
	return e
}

// mapRead400 maps a 400 on a SAFE_RETRY endpoint (/order, /build).
func mapRead400(op Operation, eb errorBody, gatewayID string) *errs.Error {
	code := errs.CodeValidationFailed
	detail := "jupiter: the provider rejected the request"
	switch {
	case matchesAny(eb.message, liquidityPatterns):
		code = errs.CodeVenueLiquidityInsufficient
		detail = "jupiter: no route or insufficient liquidity for the requested size"
	case matchesAny(eb.message, expiryPatterns):
		code = errs.CodeQuoteExpired
		detail = "jupiter: the order is no longer valid"
	}
	e := errs.New(code, detail).WithField(fieldProviderError, eb.message)
	if eb.requestID != "" {
		e = e.WithField(fieldProviderRequestID, eb.requestID)
	}
	return withGateway(e, op, http.StatusBadRequest, gatewayID)
}

// mapReadStatus maps any non-2xx final status of a SAFE_RETRY call.
func mapReadStatus(op Operation, status int, body []byte, gatewayID string, retryAfter time.Duration, attempts int) *errs.Error {
	eb := decodeErrorBody(body)
	switch {
	case status == http.StatusBadRequest:
		return mapRead400(op, eb, gatewayID)
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		e := errs.New(errs.CodeProviderUnavailable, "jupiter: the provider rejected the API credentials or permission").
			WithField(fieldProviderError, eb.message)
		return withGateway(e, op, status, gatewayID)
	case status == http.StatusNotFound:
		e := errs.New(errs.CodeProviderUnavailable, "jupiter: endpoint not found (API version drift?)")
		return withGateway(e, op, status, gatewayID)
	case status == http.StatusTooManyRequests:
		e := errs.New(errs.CodeRateLimited, "jupiter: rate limited by the provider").
			WithField(fieldProviderError, eb.raw).WithField(fieldAttempts, attempts)
		if retryAfter > 0 {
			e = e.WithRetryAfter(retryAfter)
		}
		return withGateway(e, op, status, gatewayID)
	case status >= 500:
		e := errs.New(errs.CodeProviderUnavailable, "jupiter: provider server error").
			WithField(fieldProviderError, eb.message).WithField(fieldAttempts, attempts)
		return withGateway(e, op, status, gatewayID)
	default:
		e := errs.New(errs.CodeProviderUnavailable, "jupiter: unexpected HTTP status from provider").
			WithField(fieldProviderError, eb.raw)
		return withGateway(e, op, status, gatewayID)
	}
}

// mapOrderErrorCode maps a 200 /order response whose errorCode is non-zero
// (documented: transaction is "" and errorCode explains why). Semantics
// depend on the router: aggregator 1 insufficient funds, 2 insufficient SOL
// for gas, 3 below gasless minimum; JupiterZ 1 insufficient balance, 2
// missing ATA, 3 quote build failure.
func mapOrderErrorCode(router string, code int64, msg, requestID, gatewayID string) *errs.Error {
	rfq := strings.EqualFold(router, "jupiterz")
	var e *errs.Error
	var reason string
	switch {
	case code == 1:
		reason = "INSUFFICIENT_FUNDS"
		e = errs.New(errs.CodeValidationFailed, "jupiter: the taker wallet holds insufficient input funds for this order")
	case code == 2 && !rfq:
		reason = "INSUFFICIENT_SOL_FOR_GAS"
		e = errs.New(errs.CodeValidationFailed, "jupiter: the taker wallet holds insufficient SOL for network fees")
	case code == 2 && rfq:
		reason = "MISSING_ATA"
		e = errs.New(errs.CodeValidationFailed, "jupiter: the taker wallet lacks a required token account")
	case code == 3 && !rfq:
		reason = "BELOW_GASLESS_MINIMUM"
		e = errs.New(errs.CodeValidationFailed, "jupiter: the order is below the gasless minimum")
	case code == 3 && rfq:
		reason = "RFQ_QUOTE_BUILD_FAILURE"
		e = errs.New(errs.CodeVenueUnavailable, "jupiter: the RFQ venue failed to build the quote")
	default:
		reason = "UNDOCUMENTED_ERROR_CODE"
		e = errs.New(errs.CodeValidationFailed, "jupiter: the provider reported an undocumented order error code")
	}
	e = e.WithField(fieldReason, reason).WithField(fieldProviderCode, code).WithField(fieldProviderError, msg)
	if requestID != "" {
		e = e.WithField(fieldProviderRequestID, requestID)
	}
	return withGateway(e, OpOrder, http.StatusOK, gatewayID)
}

// Documented /execute codes.
const (
	executeCodeMissingCachedOrder   int64 = -1
	executeCodeInvalidSignedTx      int64 = -2
	executeCodeInvalidMessageBytes  int64 = -3
	executeCodeAggregatorFailureMin int64 = -1004
	executeCodeAggregatorFailureMax int64 = -1000
	executeCodeRFQFailureMin        int64 = -2004
	executeCodeRFQFailureMax        int64 = -2000
)

// mapExecuteStatus maps every non-2xx /execute status. Only gateway-level
// rejections that provably happened before any broadcast are definitive
// (ASSUMED: 401/403/429/404 are answered by the gateway without reaching
// the execution backend); everything else is SUBMISSION_STATE_UNKNOWN.
func mapExecuteStatus(status int, body []byte, gatewayID, localSignature string, retryAfter time.Duration) *errs.Error {
	eb := decodeErrorBody(body)
	unknown := func(detail string) *errs.Error {
		e := errs.New(errs.CodeSubmissionStateUnknown, detail).
			WithField(fieldProviderError, eb.message).WithField(fieldSignature, localSignature).WithField(fieldSubmitted, true)
		if eb.code != nil {
			e = e.WithField(fieldProviderCode, *eb.code)
		}
		if eb.signature != "" && eb.signature != localSignature {
			e = e.WithField("provider_signature", eb.signature)
		}
		return withGateway(e, OpExecute, status, gatewayID)
	}
	switch {
	case status == http.StatusBadRequest && eb.code != nil:
		switch c := *eb.code; {
		case c == executeCodeMissingCachedOrder:
			e := errs.New(errs.CodeQuoteExpired, "jupiter: the order is no longer cached by the provider; re-quote").
				WithField(fieldProviderCode, c).WithField(fieldProviderError, eb.message).WithField(fieldSubmitted, false)
			return withGateway(e, OpExecute, status, gatewayID)
		case c == executeCodeInvalidSignedTx || c == executeCodeInvalidMessageBytes:
			e := errs.New(errs.CodeValidationFailed, "jupiter: the provider rejected the signed transaction bytes").
				WithField(fieldProviderCode, c).WithField(fieldProviderError, eb.message).WithField(fieldSubmitted, false)
			return withGateway(e, OpExecute, status, gatewayID)
		case (c >= executeCodeAggregatorFailureMin && c <= executeCodeAggregatorFailureMax) ||
			(c >= executeCodeRFQFailureMin && c <= executeCodeRFQFailureMax):
			// Documented only as "aggregator/RFQ failures": whether the
			// transaction reached the network is not stated.
			return unknown("jupiter: the provider reported an execution failure; on-chain state must be established before retrying")
		default:
			return unknown("jupiter: the provider rejected the submission with an undocumented code")
		}
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		e := errs.New(errs.CodeProviderUnavailable, "jupiter: the provider rejected the API credentials or permission").
			WithField(fieldProviderError, eb.message).WithField(fieldSubmitted, false)
		return withGateway(e, OpExecute, status, gatewayID)
	case status == http.StatusNotFound:
		e := errs.New(errs.CodeProviderUnavailable, "jupiter: execute endpoint not found (API version drift?)").WithField(fieldSubmitted, false)
		return withGateway(e, OpExecute, status, gatewayID)
	case status == http.StatusTooManyRequests:
		e := errs.New(errs.CodeRateLimited, "jupiter: rate limited by the provider; the transaction was not accepted").
			WithField(fieldProviderError, eb.raw).WithField(fieldSubmitted, false)
		if retryAfter > 0 {
			e = e.WithRetryAfter(retryAfter)
		}
		return withGateway(e, OpExecute, status, gatewayID)
	case status >= 500:
		return unknown("jupiter: provider server error during submission; landing state unknown")
	default:
		return unknown("jupiter: unexpected HTTP status during submission; landing state unknown")
	}
}

// submissionUnknown builds the SUBMISSION_STATE_UNKNOWN error for transport
// failures and ambiguous bodies.
func submissionUnknown(cause error, detail, localSignature, gatewayID string, timedOut bool) *errs.Error {
	e := errs.Wrap(cause, errs.CodeSubmissionStateUnknown, detail).
		WithField(fieldSignature, localSignature).WithField(fieldSubmitted, true).WithField(fieldTimedOut, timedOut)
	return withGateway(e, OpExecute, 0, gatewayID)
}
