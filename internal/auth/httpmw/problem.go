package httpmw

import (
	"encoding/json"
	"net/http"
)

// Stable machine-readable codes written by this package. They mirror the
// errs.Code constants so the API layer can switch to errs.ToProblem
// without changing clients.
const (
	CodeUnauthenticated = "UNAUTHENTICATED"
	CodeForbidden       = "FORBIDDEN"
	CodeStepUpRequired  = "STEP_UP_REQUIRED"
	CodeInternal        = "INTERNAL"
)

// problem is an RFC 9457 application/problem+json body.
type problem struct {
	Type      string `json:"type"`
	Title     string `json:"title"`
	Status    int    `json:"status"`
	Detail    string `json:"detail,omitempty"`
	Instance  string `json:"instance,omitempty"`
	Code      string `json:"code"`
	RequestID string `json:"request_id,omitempty"`
}

// writeProblem renders a problem response. detail must be safe for
// clients: never internal error text, never a token.
func writeProblem(w http.ResponseWriter, r *http.Request, status int, code, detail string) {
	p := problem{
		Type:      "about:blank",
		Title:     http.StatusText(status),
		Status:    status,
		Detail:    detail,
		Code:      code,
		RequestID: r.Header.Get("X-Request-Id"),
	}
	if r.URL != nil {
		p.Instance = r.URL.Path
	}
	h := w.Header()
	h.Set("Content-Type", "application/problem+json")
	h.Set("Cache-Control", "no-store")
	if status == http.StatusUnauthorized {
		// RFC 9110 requires a challenge; cookie sessions have no scheme
		// clients can act on, so name a private one that never prompts.
		h.Set("WWW-Authenticate", `Session realm="controlplane"`)
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(p)
}
