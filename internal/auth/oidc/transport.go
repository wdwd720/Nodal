package oidc

import (
	"fmt"
	"io"
	"net/http"
)

// MaxDocumentBytes caps every response body read from the issuer. Discovery
// documents, JWKS and token responses are a few kilobytes; anything larger is
// a misconfigured or hostile endpoint.
const MaxDocumentBytes = 1 << 20

var errDocumentTooLarge = fmt.Errorf("oidc: response body exceeds %d bytes", MaxDocumentBytes)

// boundedClient returns a copy of base whose transport caps every response
// body at MaxDocumentBytes and which, when base has no timeout, times out
// after DefaultHTTPTimeout. base itself is not modified. go-oidc and
// golang.org/x/oauth2 both take the client from the context
// (oidc.ClientContext), so one wrapped client covers discovery, JWKS and the
// token endpoint.
func boundedClient(base *http.Client) *http.Client {
	c := *base
	if c.Timeout == 0 {
		c.Timeout = DefaultHTTPTimeout
	}
	next := c.Transport
	if next == nil {
		next = http.DefaultTransport
	}
	c.Transport = &boundedTransport{next: next, limit: MaxDocumentBytes}
	return &c
}

type boundedTransport struct {
	next  http.RoundTripper
	limit int64
}

func (t *boundedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.next.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	resp.Body = &boundedBody{ReadCloser: resp.Body, remaining: t.limit}
	return resp, nil
}

// boundedBody fails the read, rather than truncating silently, once more
// than the limit has been produced.
type boundedBody struct {
	io.ReadCloser
	remaining int64
}

func (b *boundedBody) Read(p []byte) (int, error) {
	if b.remaining < 0 {
		return 0, errDocumentTooLarge
	}
	n, err := b.ReadCloser.Read(p)
	b.remaining -= int64(n)
	if b.remaining < 0 {
		return 0, errDocumentTooLarge
	}
	return n, err
}
