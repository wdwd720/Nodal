package agent

import (
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/nodal/controlplane/internal/errs"
)

// EgressError is returned by the guarded transport when a tool adapter tries
// to reach a host that is not on its tools.egress_hosts allowlist. It is a
// typed error rather than a log line: the run records the refusal.
type EgressError struct {
	Tool string
	Host string
}

func (e *EgressError) Error() string {
	return "agent: tool " + e.Tool + " may not connect to " + e.Host
}

// EgressTransport refuses every request whose host is not on the allowlist,
// before the connection is made. There is no generic HTTP tool: an adapter
// receives only this transport, so an adapter that has been compromised, or
// a provider that redirects, still cannot reach an arbitrary host.
//
// An empty allowlist blocks everything. Redirects are checked too, because
// the client re-enters RoundTrip for each hop.
type EgressTransport struct {
	tool    string
	allowed map[string]struct{}
	base    http.RoundTripper
}

var _ http.RoundTripper = (*EgressTransport)(nil)

// NewEgressTransport wraps base so it can only reach hosts. Host matching is
// exact and case-insensitive on the hostname; a port in the request is
// ignored for matching but an allowlist entry may itself carry a port, in
// which case the port must match too.
func NewEgressTransport(tool string, hosts []string, base http.RoundTripper) *EgressTransport {
	if base == nil {
		base = http.DefaultTransport
	}
	allowed := make(map[string]struct{}, len(hosts))
	for _, h := range hosts {
		h = strings.ToLower(strings.TrimSpace(h))
		if h != "" {
			allowed[h] = struct{}{}
		}
	}
	return &EgressTransport{tool: tool, allowed: allowed, base: base}
}

// RoundTrip refuses a disallowed host without dialing.
func (t *EgressTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req == nil || req.URL == nil {
		return nil, &EgressError{Tool: t.tool, Host: ""}
	}
	if !t.Allows(req.URL.Host) {
		return nil, &EgressError{Tool: t.tool, Host: req.URL.Host}
	}
	return t.base.RoundTrip(req)
}

// Allows reports whether hostPort is permitted. It is exported so an adapter
// that dials outside net/http (a websocket, a gRPC channel) can apply the
// same rule.
func (t *EgressTransport) Allows(hostPort string) bool {
	if hostPort == "" {
		return false
	}
	full := strings.ToLower(hostPort)
	if _, ok := t.allowed[full]; ok {
		return true
	}
	host := full
	if h, _, err := net.SplitHostPort(full); err == nil {
		host = h
	}
	_, ok := t.allowed[host]
	return ok
}

// IsEgressRefused reports whether err came from the egress allowlist.
func IsEgressRefused(err error) bool {
	var e *EgressError
	return asEgressError(err, &e)
}

func asEgressError(err error, target **EgressError) bool {
	for err != nil {
		if e, ok := err.(*EgressError); ok { //nolint:errorlint // walked explicitly below
			*target = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// egressRefusal converts an egress error into the platform's typed form.
func egressRefusal(err error) *errs.Error {
	var e *EgressError
	if asEgressError(err, &e) {
		return errs.Newf(errs.CodeForbidden, "agent: egress refused for tool %s", e.Tool).
			WithField("tool", e.Tool).WithField("host", e.Host)
	}
	return errs.Wrap(err, errs.CodeForbidden, "agent: egress refused")
}

// NewToolHTTPClient builds the only HTTP client an adapter for t may use: an
// egress-guarded transport with the tool's own timeout. Redirects are capped
// and each hop is re-checked by the transport.
func NewToolHTTPClient(t Tool, base http.RoundTripper) *http.Client {
	timeout := t.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &http.Client{
		Transport: NewEgressTransport(t.Key(), t.EgressHosts, base),
		Timeout:   timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return errs.New(errs.CodeProviderUnavailable, "agent: too many redirects")
			}
			return nil
		},
	}
}
