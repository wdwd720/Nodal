package errs

import (
	"encoding/json"
	"maps"
	"math"
	"net/http"
	"strconv"
	"time"
)

// ContentType is the media type of a Problem response (RFC 9457).
const ContentType = "application/problem+json"

// internalDetail is the only detail ever sent for INTERNAL errors.
const internalDetail = "internal error"

// Problem is the RFC 9457 application/problem+json document, extended with
// the stable code, structured fields and the request id so a client can
// quote it back to support.
type Problem struct {
	Type      string         `json:"type"`
	Title     string         `json:"title"`
	Status    int            `json:"status"`
	Detail    string         `json:"detail"`
	Instance  string         `json:"instance,omitempty"`
	Code      Code           `json:"code"`
	Fields    map[string]any `json:"fields,omitempty"`
	RequestID string         `json:"request_id,omitempty"`

	// RetryAfter, when set, is emitted by WriteProblem as the Retry-After
	// header. It is not part of the JSON body.
	RetryAfter *time.Duration `json:"-"`
}

// ToProblem projects err to a Problem for instance (the request path) and
// requestID.
//
// Redaction rules, applied unconditionally:
//   - A nil error, a non-*Error, an *Error with an unregistered code and an
//     *Error with CodeInternal all become an INTERNAL problem whose detail
//     is the constant "internal error" and whose fields are empty.
//   - For every other code the detail is the Error's Detail (falling back
//     to the code's title when empty) and Fields is a copy. The cause chain
//     is never consulted.
//
// IDEMPOTENCY_IN_PROGRESS and RATE_LIMITED default RetryAfter to one second
// when the error did not specify one, so those responses always carry a
// Retry-After header.
func ToProblem(err error, instance, requestID string) Problem {
	e, ok := As(err)
	code := CodeInternal
	if ok && e.Code.Known() {
		code = e.Code
	}
	p := Problem{
		Type:      ProblemType(code),
		Title:     Title(code),
		Status:    HTTPStatus(code),
		Instance:  instance,
		Code:      code,
		RequestID: requestID,
	}
	if code == CodeInternal {
		p.Detail = internalDetail
		return p
	}
	p.Detail = e.Detail
	if p.Detail == "" {
		p.Detail = p.Title
	}
	if len(e.Fields) > 0 {
		p.Fields = maps.Clone(e.Fields)
	}
	switch {
	case e.RetryAfter != nil:
		d := *e.RetryAfter
		p.RetryAfter = &d
	case code == CodeIdempotencyInProgress || code == CodeRateLimited:
		d := time.Second
		p.RetryAfter = &d
	}
	return p
}

// WriteProblem writes p as an application/problem+json response with the
// status p.Status (or the status of p.Code when zero) and, if RetryAfter is
// set, a Retry-After header in whole seconds rounded up.
//
// The body is marshaled before any header is written; if marshaling fails
// (for example a field value that encoding/json cannot represent) a plain
// INTERNAL problem with the same instance and request id is sent instead,
// so a bad field can neither leak nor produce a half-written response.
func WriteProblem(w http.ResponseWriter, p Problem) {
	if p.Status == 0 {
		p.Status = HTTPStatus(p.Code)
	}
	body, err := json.Marshal(p)
	if err != nil {
		p = Problem{
			Type:      ProblemType(CodeInternal),
			Title:     Title(CodeInternal),
			Status:    HTTPStatus(CodeInternal),
			Detail:    internalDetail,
			Instance:  p.Instance,
			Code:      CodeInternal,
			RequestID: p.RequestID,
		}
		body, _ = json.Marshal(p) // cannot fail: only strings and ints
	}

	h := w.Header()
	h.Set("Content-Type", ContentType)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "no-store")
	if p.RetryAfter != nil && *p.RetryAfter > 0 {
		h.Set("Retry-After", strconv.FormatInt(retryAfterSeconds(*p.RetryAfter), 10))
	}
	w.WriteHeader(p.Status)
	_, _ = w.Write(body)
}

// WriteError is ToProblem followed by WriteProblem.
func WriteError(w http.ResponseWriter, err error, instance, requestID string) {
	WriteProblem(w, ToProblem(err, instance, requestID))
}

// retryAfterSeconds converts d to whole seconds, rounding up and never
// below one so the header is always a positive delay.
func retryAfterSeconds(d time.Duration) int64 {
	s := int64(math.Ceil(d.Seconds()))
	if s < 1 {
		s = 1
	}
	return s
}
