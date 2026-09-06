package jupiter

import (
	"context"
	"net/http"
	"time"

	"github.com/nodal/controlplane/internal/observability"
)

// Evidence is the raw request/response record of one HTTP attempt. It is
// handed to RawArchive before the response is interpreted (PART 183), so
// that every financial decision can be traced to the exact bytes the
// provider sent, independent of how this package parsed them.
type Evidence struct {
	Provider  string
	Operation Operation
	Attempt   int

	RequestMethod string
	RequestURL    string
	// RequestHeaders is a redacted copy: the API key and every other
	// denylisted header value are replaced by the redaction marker.
	RequestHeaders http.Header
	RequestBody    []byte

	// ResponseStatus is 0 and TransportError non-empty when no response was
	// received (timeout, connection failure).
	ResponseStatus  int
	ResponseHeaders http.Header
	ResponseBody    []byte
	TransportError  string
	// GatewayRequestID is the documented x-api-gateway-request-id.
	GatewayRequestID string

	SentAt     time.Time
	ReceivedAt time.Time
	// ResponseHash is SHA-256 of ResponseBody (of the empty body when none).
	ResponseHash []byte
}

// RawArchive stores Evidence durably and returns a reference (for example
// an object key) that is recorded on the returned Order / ExecuteResult /
// BuildResult as RawRef. Implementations must never mutate the Evidence
// and must never write the API key (the headers are already redacted).
type RawArchive interface {
	Archive(ctx context.Context, ev Evidence) (ref string, err error)
}

// SecurityEvent describes a security-relevant provider response.
type SecurityEvent struct {
	// Kind is a stable identifier, e.g. SecurityEventProviderAuthRejected.
	Kind      string
	Provider  string
	Operation Operation
	// HTTPStatus is 401 or 403.
	HTTPStatus       int
	GatewayRequestID string
	// Detail is a client-safe description without secrets.
	Detail string
	At     time.Time
}

// Security event kinds.
const (
	// SecurityEventProviderAuthRejected: the gateway rejected the API key
	// (401) or the key lacks the Swap permission / a firewall blocked the
	// call (403). Either means the integration is misconfigured or the key
	// was revoked, which operators must see.
	SecurityEventProviderAuthRejected = "PROVIDER_AUTH_REJECTED"
)

// SecurityEventSink receives security events. Implementations must not
// block for long; the client calls it synchronously on the request path.
type SecurityEventSink interface {
	SecurityEvent(ctx context.Context, ev SecurityEvent)
}

// redactHeaders returns a copy of h with denylisted header values replaced
// by the redaction marker. The API key header is always denylisted.
func redactHeaders(h http.Header) http.Header {
	out := make(http.Header, len(h))
	for k, vs := range h {
		if observability.IsDeniedKey(k) {
			out[k] = []string{observability.RedactedMarker}
			continue
		}
		out[k] = append([]string(nil), vs...)
	}
	return out
}
