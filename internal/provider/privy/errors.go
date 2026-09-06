package privy

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	privyclient "github.com/privy-io/go-sdk"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/provider"
)

// Field names on mapped errors.
const (
	FieldProvider   = "provider"
	FieldOperation  = "operation"
	FieldRetryClass = "retry_class"
	FieldStatus     = "provider_status"
)

// RetryClassOf returns the retry class recorded on an error produced by this
// adapter, or false if err did not come from it.
func RetryClassOf(err error) (provider.RetryClass, bool) {
	e, ok := errs.As(err)
	if !ok || e.Fields == nil {
		return "", false
	}
	rc, ok := e.Fields[FieldRetryClass].(provider.RetryClass)
	return rc, ok
}

// mapError converts an SDK/transport error into an *errs.Error with a stable
// code. The raw response body is never included: only the HTTP status and,
// when the body carries a recognizable machine code, that code.
func mapError(op string, class provider.RetryClass, err error) error {
	if err == nil {
		return nil
	}
	with := func(e *errs.Error, status int) error {
		e = e.WithField(FieldProvider, Name).WithField(FieldOperation, op).WithField(FieldRetryClass, class)
		if status != 0 {
			e = e.WithField(FieldStatus, status)
		}
		return e
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return with(errs.Wrap(err, errs.CodeProviderUnavailable, "privy: request timed out or was cancelled"), 0)
	}
	var apiErr *privyclient.Error
	if errors.As(err, &apiErr) {
		status := apiErr.StatusCode
		if status == 0 && apiErr.Response != nil {
			status = apiErr.Response.StatusCode
		}
		code := bodyCode(apiErr.RawJSON())
		switch {
		case code == "POLICY_VIOLATION" || strings.Contains(strings.ToLower(code), "policy"):
			return with(errs.Wrap(sanitized(apiErr), errs.CodeSigningRejected, "privy: provider policy rejected the request"), status)
		case status == http.StatusTooManyRequests:
			e := errs.Wrap(sanitized(apiErr), errs.CodeRateLimited, "privy: rate limited")
			if ra := retryAfter(apiErr.Response); ra > 0 {
				e = e.WithRetryAfter(ra)
			}
			return with(e, status)
		case status == http.StatusUnauthorized:
			return with(errs.Wrap(sanitized(apiErr), errs.CodeUnauthenticated, "privy: provider rejected the app credentials"), status)
		case status == http.StatusForbidden:
			return with(errs.Wrap(sanitized(apiErr), errs.CodeForbidden, "privy: provider refused the authorization"), status)
		case status == http.StatusNotFound:
			return with(errs.Wrap(sanitized(apiErr), errs.CodeNotFound, "privy: resource not found at the provider"), status)
		case status == http.StatusConflict:
			return with(errs.Wrap(sanitized(apiErr), errs.CodeConflict, "privy: conflicting request (idempotency key reused with a different body?)"), status)
		case status == http.StatusBadRequest || status == http.StatusUnprocessableEntity:
			return with(errs.Wrap(sanitized(apiErr), errs.CodeValidationFailed, "privy: provider rejected the request as invalid"), status)
		case status >= 500:
			return with(errs.Wrap(sanitized(apiErr), errs.CodeProviderUnavailable, "privy: provider error"), status)
		default:
			return with(errs.Wrap(sanitized(apiErr), errs.CodeInternal, "privy: unexpected provider response"), status)
		}
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return with(errs.Wrap(err, errs.CodeProviderUnavailable, "privy: transport failure"), 0)
	}
	return with(errs.Wrap(err, errs.CodeInternal, "privy: unexpected provider response"), 0)
}

// sanitized keeps only the status line of an API error as the cause so the
// body (which may echo request material) never reaches logs verbatim.
func sanitized(e *privyclient.Error) error {
	status := e.StatusCode
	if status == 0 && e.Response != nil {
		status = e.Response.StatusCode
	}
	return errors.New("privy api status " + strconv.Itoa(status) + " code " + strconv.Quote(bodyCode(e.RawJSON())))
}

// bodyCode extracts a machine-readable code from an undocumented error body
// ({"code": "...", "error": "..."} shapes are the ones observed); it returns
// "" when nothing recognizable is present.
func bodyCode(raw string) string {
	for _, key := range []string{`"code":"`, `"error_code":"`, `"error":"`} {
		i := strings.Index(raw, key)
		if i < 0 {
			continue
		}
		rest := raw[i+len(key):]
		j := strings.IndexByte(rest, '"')
		if j < 0 {
			continue
		}
		v := rest[:j]
		if len(v) > 64 {
			v = v[:64]
		}
		// Only keep values that look like machine codes; free-text messages
		// stay out of Detail and Fields.
		if strings.ContainsAny(v, " ") {
			if strings.HasPrefix(strings.ToUpper(v), "POLICY") {
				return "POLICY_VIOLATION"
			}
			continue
		}
		return v
	}
	return ""
}

func retryAfter(resp *http.Response) time.Duration {
	if resp == nil {
		return 0
	}
	v := resp.Header.Get("Retry-After")
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	return 0
}
